package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	gonet "net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/telemetry"
)

var (
	mmdsEndpoint   = flag.String("mmds-endpoint", "http://169.254.169.254", "MMDS endpoint")
	workspaceDir   = flag.String("workspace-dir", "/workspace", "Workspace directory")
	runnerDir      = flag.String("ci-runner-dir", "/home/sandbox-user", "CI runner working directory")
	runnerUsername = flag.String("sandbox-user", "runner", "Username for the sandbox user and file ownership (e.g., 'runner' or 'gleanuser')")
	logLevel       = flag.String("log-level", "info", "Log level")
	readyFile      = flag.String("ready-file", "/var/run/thaw-agent/ready", "Ready signal file")
	skipNetwork    = flag.Bool("skip-network", false, "Skip network configuration")
)

// init registers backwards-compatible flag aliases for renamed flags.
func init() {
	flag.StringVar(runnerDir, "runner-dir", "/home/sandbox-user", "Deprecated: use --ci-runner-dir")
	flag.StringVar(runnerUsername, "runner-user", "runner", "Deprecated: use --sandbox-user")
}

// WarmupState tracks the current warmup progress (for snapshot building)
type WarmupState struct {
	Phase            string    `json:"phase"`
	Message          string    `json:"message,omitempty"`
	Error            string    `json:"error,omitempty"`
	Complete         bool      `json:"complete"`
	StartedAt        time.Time `json:"started_at"`
	CompletedAt      time.Time `json:"completed_at,omitempty"`
	Duration         string    `json:"duration,omitempty"`
	ExternalsFetched int       `json:"externals_fetched,omitempty"`
}

// WarmupLogBuffer is a thread-safe ring buffer for streaming warmup command output
type WarmupLogBuffer struct {
	mu    sync.Mutex
	lines []string
	seq   int64 // monotonic sequence number
}

func (b *WarmupLogBuffer) Add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	b.lines = append(b.lines, line)
	// Keep last 500 lines
	if len(b.lines) > 500 {
		b.lines = b.lines[len(b.lines)-500:]
	}
}

// Since returns lines added after the given sequence number
func (b *WarmupLogBuffer) Since(afterSeq int64) ([]string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if afterSeq >= b.seq {
		return nil, b.seq
	}
	// Calculate how many new lines
	total := b.seq
	count := int(total - afterSeq)
	if count > len(b.lines) {
		count = len(b.lines)
	}
	result := make([]string, count)
	copy(result, b.lines[len(b.lines)-count:])
	return result, total
}

var globalWarmupLogs = &WarmupLogBuffer{}

// cgroupMgr is the cgroup v2 manager for user process isolation. Nil if cgroup
// v2 is not available (graceful degradation).
var cgroupMgr *cgroupManager

// globalLogBuffer captures all logrus log entries for the /logs HTTP endpoint.
// This allows debugging thaw-agent behavior from the host via:
//
//	curl http://<vm-ip>:10501/logs
var globalLogBuffer = &WarmupLogBuffer{}

// logCaptureHook is a logrus hook that captures log entries into globalLogBuffer.
type logCaptureHook struct{}

func (h *logCaptureHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *logCaptureHook) Fire(entry *logrus.Entry) error {
	msg := fmt.Sprintf("[%s] %s: %s", entry.Time.Format("15:04:05"), entry.Level.String(), entry.Message)
	for k, v := range entry.Data {
		if k == "component" {
			continue
		}
		msg += fmt.Sprintf(" %s=%v", k, v)
	}
	globalLogBuffer.Add(msg)
	return nil
}

var globalWarmupState = &WarmupState{
	Phase:     "initializing",
	StartedAt: time.Now(),
}


// MMDSData represents the data structure from MMDS
type MMDSData struct {
	Latest struct {
		Meta struct {
			RunnerID     string `json:"runner_id"`
			HostID       string `json:"host_id"`
			InstanceName string `json:"instance_name,omitempty"`
			Environment  string `json:"environment"`
			JobID        string `json:"job_id,omitempty"`
			Mode         string `json:"mode,omitempty"`         // "warmup" for snapshot building, empty for normal runner
			CurrentTime  string `json:"current_time,omitempty"` // RFC3339 timestamp from host for clock sync
		} `json:"meta"`
		Network struct {
			IP        string `json:"ip"`
			Gateway   string `json:"gateway"`
			Netmask   string `json:"netmask"`
			DNS       string `json:"dns"`
			Interface string `json:"interface"`
			MAC       string `json:"mac"`
		} `json:"network"`
		Job struct {
			Repo          string            `json:"repo"`
			Branch        string            `json:"branch"`
			Commit        string            `json:"commit"`
			GitToken      string            `json:"git_token"`
			Labels        map[string]string `json:"labels"`
		} `json:"job"`
		Snapshot struct {
			Version string `json:"version"`
		} `json:"snapshot"`
		Drives []snapshot.DriveSpec `json:"drives,omitempty"`
		Exec   struct {
			Command    []string          `json:"command,omitempty"`
			Env        map[string]string `json:"env,omitempty"`
			WorkingDir string            `json:"working_dir,omitempty"`
			TimeoutSec int               `json:"timeout_seconds,omitempty"`
		} `json:"exec,omitempty"`
		Warmup struct {
			Commands []snapshot.SnapshotCommand `json:"commands,omitempty"`
		} `json:"warmup,omitempty"`
		// Mirrors snapshot.StartCommand — keep in sync with pkg/snapshot/start_command.go.
		StartCommand struct {
			Command    []string          `json:"command,omitempty"`
			Port       int               `json:"port,omitempty"`
			HealthPath string            `json:"health_path,omitempty"`
			Env        map[string]string `json:"env,omitempty"`
			RunAs      string            `json:"run_as,omitempty"`
		} `json:"start_command,omitempty"`
	} `json:"latest"`
}

var log *logrus.Logger
var metrics *telemetry.StructuredLogger
var bootTimer *telemetry.Timer

func main() {
	// Top-level panic recovery: keep the process alive so the health server
	// (:10501) remains accessible for debugging even if initialization panics.
	defer func() {
		if r := recover(); r != nil {
			if log != nil {
				log.WithField("panic", fmt.Sprintf("%v", r)).Error("Thaw agent panicked - keeping process alive for debugging")
				globalLogBuffer.Add(fmt.Sprintf("[PANIC] %v", r))
			} else {
				fmt.Fprintf(os.Stderr, "PANIC: %v\n", r)
			}
			// Block forever so health server stays up
			select {}
		}
	}()

	flag.Parse()

	// Setup logger
	log = logrus.New()
	log.SetFormatter(&logrus.JSONFormatter{})
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	log.SetLevel(level)
	log.AddHook(&logCaptureHook{})

	// Start boot timer immediately
	bootTimer = telemetry.NewTimer()

	log.Info("Thaw agent starting...")

	// Load Docker image ENV variables from /etc/environment into the current
	// process environment. The snapshot-builder writes these during the
	// Docker-to-rootfs conversion so that start_command, shell commands, and
	// other child processes inherit the PATH, PYTHONPATH, etc. that the
	// Docker image defined via ENV directives.
	loadDockerEnv()

	// Initialize cgroup v2 isolation for user processes. This creates agent/
	// and user/ sub-cgroups and moves the thaw-agent into agent/. User commands
	// from /exec and /pty are placed into user/ to prevent resource starvation.
	cgroupMgr = initCgroup()

	// Track progress for debugging
	currentStep := "starting"
	var stepMutex sync.Mutex
	setStep := func(step string) {
		stepMutex.Lock()
		currentStep = step
		stepMutex.Unlock()
		log.WithField("step", step).Info("Boot progress")
	}

	// Start a basic health server immediately (for debugging)
	// This allows us to verify the agent is alive even if MMDS fails
	go func() {
		http.HandleFunc("/alive", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("thaw-agent alive"))
		})
		http.HandleFunc("/progress", func(w http.ResponseWriter, r *http.Request) {
			stepMutex.Lock()
			step := currentStep
			stepMutex.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"step": step})
		})
		http.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
			lines, _ := globalLogBuffer.Since(0)
			w.Header().Set("Content-Type", "text/plain")
			for _, line := range lines {
				fmt.Fprintln(w, line)
			}
		})
		http.HandleFunc("/exec", execHandler)
		http.HandleFunc("/pty", ptyHandler)
		http.HandleFunc("/service-logs", serviceLogsHandler)
		registerFileHandlers()
		if err := http.ListenAndServe(":10501", nil); err != nil {
			log.WithError(err).Debug("Early health server failed")
		}
	}()
	setStep("early_health_started")

	// Network is configured by kernel boot parameters (ip=...), so we just need
	// to wait briefly for the interface to be ready
	time.Sleep(100 * time.Millisecond)
	setStep("waiting_for_mmds")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Wait for MMDS to be available
	log.Info("Waiting for MMDS...")
	mmdsData, err := waitForMMDS(ctx)
	if err != nil {
		log.WithError(err).Fatal("Failed to get MMDS data")
	}

	bootTimer.Phase("mmds_wait")
	setStep("mmds_received")
	log.WithFields(logrus.Fields{
		"runner_id": mmdsData.Latest.Meta.RunnerID,
		"host_id":   mmdsData.Latest.Meta.HostID,
		"job_id":    mmdsData.Latest.Meta.JobID,
		"repo":      mmdsData.Latest.Job.Repo,
		"branch":    mmdsData.Latest.Job.Branch,
	}).Info("MMDS data received")

	// Initialize structured metrics logger for GCP log-based metrics
	metrics = telemetry.NewStructuredLogger(log, "thaw-agent", mmdsData.Latest.Meta.RunnerID)

	// Mount extension drives declared by the workload config.
	setStep("extension_drives")
	mountExtensionDrives(mmdsData)
	bootTimer.Phase("extension_drives")

	// Configure network
	setStep("network_config")
	if !*skipNetwork {
		log.Info("Configuring network...")
		if err := configureNetwork(mmdsData); err != nil {
			log.WithError(err).Error("Failed to configure network")
		}
	}
	bootTimer.Phase("network_config")

	// Regenerate hostname
	log.Info("Regenerating hostname...")
	if err := regenerateHostname(mmdsData.Latest.Meta.RunnerID); err != nil {
		log.WithError(err).Warn("Failed to regenerate hostname")
	}
	bootTimer.Phase("hostname")

	// Resync clock
	log.Info("Resyncing clock...")
	if err := resyncClock(mmdsData); err != nil {
		log.WithError(err).Warn("Failed to resync clock")
	}
	bootTimer.Phase("clock_sync")

	// Check if we're in warmup mode (for snapshot building)
	if mmdsData.Latest.Meta.Mode == "warmup" {
		log.Info("Running in WARMUP mode for snapshot building")

		// Start health server in background FIRST so snapshot-builder can poll us
		go startHealthServer(mmdsData)
		log.Info("Health server started in background for warmup mode")

		// Run warmup process (blocking until complete)
		if err := runWarmupMode(mmdsData); err != nil {
			globalWarmupState.Error = err.Error()
			globalWarmupState.Phase = "failed"
			log.WithError(err).Error("Warmup failed")
		} else {
			globalWarmupState.Complete = true
			globalWarmupState.Phase = "complete"
			globalWarmupState.CompletedAt = time.Now()
			globalWarmupState.Duration = time.Since(globalWarmupState.StartedAt).String()
			log.Info("Warmup completed successfully")
		}

		// Signal ready
		if err := signalReady(); err != nil {
			log.WithError(err).Error("Failed to signal ready")
		}

		// Wait for snapshot to be taken and mode to change
		// After snapshot restore, MMDS will have new data with mode != "warmup"
		log.Info("Warmup complete, polling MMDS for mode change (snapshot restore)...")
		pollCount := 0
		pollInterval := 10 * time.Millisecond
		for {
			time.Sleep(pollInterval)
			if pollInterval < 100*time.Millisecond {
				pollInterval *= 2
			}
			pollCount++

			newData, err := fetchMMDSData()
			if err != nil {
				if pollCount%20 == 0 { // Log every 10 seconds
					log.WithError(err).WithField("poll_count", pollCount).Info("Failed to fetch MMDS during restore poll")
				}
				continue
			}

			// Log what we got every 10 seconds for debugging
			if pollCount%20 == 0 {
				log.WithFields(logrus.Fields{
					"poll_count": pollCount,
					"mode":       newData.Latest.Meta.Mode,
					"runner_id":  newData.Latest.Meta.RunnerID,
				}).Info("MMDS poll result")
			}

			// Check if mode changed from warmup OR runner_id changed
			// (runner_id change with mode still "warmup" = incremental re-warmup)
			if newData.Latest.Meta.Mode != "warmup" ||
				newData.Latest.Meta.RunnerID != mmdsData.Latest.Meta.RunnerID {
				if newData.Latest.Meta.Mode == "warmup" {
					// Incremental re-warmup: runner_id changed but mode still warmup
					mmdsData.Latest = newData.Latest
					log.WithField("new_runner_id", newData.Latest.Meta.RunnerID).Info("Re-warmup: incremental snapshot build detected")

					// Reconfigure network before running warmup commands.
					// The snapshot's network state may be stale (empty resolv.conf, broken routes).
					if !*skipNetwork {
						tempData := &MMDSData{}
						tempData.Latest = newData.Latest
						log.Info("Re-warmup: reconfiguring network...")
						if err := configureNetwork(tempData); err != nil {
							log.WithError(err).Error("Re-warmup: network reconfig failed")
						} else {
							log.Info("Re-warmup: network reconfigured successfully")
						}
					}

					globalWarmupState = &WarmupState{StartedAt: time.Now()}
					if err := runWarmupMode(mmdsData); err != nil {
						globalWarmupState.Error = err.Error()
						globalWarmupState.Phase = "failed"
						log.WithError(err).Error("Re-warmup failed")
					} else {
						globalWarmupState.Complete = true
						globalWarmupState.Phase = "complete"
						globalWarmupState.CompletedAt = time.Now()
						globalWarmupState.Duration = time.Since(globalWarmupState.StartedAt).String()
						log.Info("Re-warmup completed successfully")
					}
					if err := signalReady(); err != nil {
						log.WithError(err).Error("Failed to signal ready after re-warmup")
					}
					pollInterval = 10 * time.Millisecond // Reset poll interval
					continue                             // Keep polling for next restore
				}
			}

			if newData.Latest.Meta.Mode != "warmup" {
				log.WithFields(logrus.Fields{
					"old_mode":      "warmup",
					"new_mode":      newData.Latest.Meta.Mode,
					"new_runner_id": newData.Latest.Meta.RunnerID,
				}).Info("Detected snapshot restore - mode changed, continuing to runner mode")
				// Update the existing mmdsData in-place so the health server sees the new data
				// (the health server has a reference to the original mmdsData)
				mmdsData.Latest = newData.Latest

				// Use a temporary MMDSData wrapper for the helper functions
				tempData := &MMDSData{}
				tempData.Latest = newData.Latest

				// Reconfigure network for new slot
				// Bug fix: Snapshot bakes slot-0's IP via kernel boot params.
				// After restore on slot N, the guest has the wrong IP.
				if !*skipNetwork {
					log.Info("Reconfiguring network after snapshot restore...")
					if err := configureNetwork(tempData); err != nil {
						log.WithError(err).Error("Post-restore network reconfig failed")
					} else {
						log.Info("Post-restore network reconfigured successfully")
					}
				}

				// Sync clock from MMDS current_time after snapshot restore.
				// The host sets current_time when building MMDS data. Without this,
				// the guest clock is stuck at snapshot creation time.
				if ct := newData.Latest.Meta.CurrentTime; ct != "" {
					if hostTime, err := time.Parse(time.RFC3339, ct); err == nil {
						tv := syscall.Timeval{Sec: hostTime.Unix()}
						if err := syscall.Settimeofday(&tv); err == nil {
							log.WithField("server_time", hostTime.UTC().Format(time.RFC3339)).Info("Clock synced from MMDS after snapshot restore")
						} else {
							formatted := hostTime.UTC().Format("2006-01-02 15:04:05")
							exec.Command("date", "-u", "-s", formatted).Run()
							log.WithField("server_time", hostTime.UTC().Format(time.RFC3339)).Info("Clock synced via date command after snapshot restore")
						}
					}
				}

				// Reset boot timer so phase durations reflect post-restore time,
				// not accumulated warmup time from before the snapshot.
				bootTimer = telemetry.NewTimer()

				break
			}
		}

		// Fall through to normal runner mode
	} else if mmdsData.Latest.Meta.Mode == "exec" {
		// Exec mode: run start_command if configured, then idle
		go startHealthServer(mmdsData)
		// Run start_command before signaling ready
		if len(mmdsData.Latest.StartCommand.Command) > 0 {
			if err := runStartCommand(mmdsData); err != nil {
				log.WithError(err).Error("Failed to start user service")
			}
		}
		if err := signalReady(); err != nil {
			log.WithError(err).Error("Failed to signal ready")
		}
		log.WithFields(logrus.Fields{
			"runner_id": mmdsData.Latest.Meta.RunnerID,
			"boot_ms":   bootTimer.Total().Milliseconds(),
		}).Info("Exec mode: ready for commands")
		if metrics != nil {
			metrics.LogBootComplete(bootTimer)
		}
		select {} // block forever, serve /exec requests and user service
	}

	// Normal runner mode
	setStep("starting_health_server")
	// Start health server in background FIRST so we can always monitor the VM
	go startHealthServer(mmdsData)
	log.Info("Health server started in background")

	// Run start_command if configured (before CI registration)
	if len(mmdsData.Latest.StartCommand.Command) > 0 {
		setStep("start_command")
		if err := runStartCommand(mmdsData); err != nil {
			log.WithError(err).Error("Failed to start user service")
		}
		bootTimer.Phase("start_command")
	}

	// Signal ready
	log.Info("Signaling ready...")
	if err := signalReady(); err != nil {
		log.WithError(err).Error("Failed to signal ready")
	}
	bootTimer.Stop()

	// Log boot completion metrics
	if metrics != nil {
		metrics.LogBootComplete(bootTimer)
		metrics.LogDuration(telemetry.MetricVMReadyDuration, bootTimer.Total(), nil)
	}

	log.WithFields(logrus.Fields{
		"total_ms": bootTimer.Total().Milliseconds(),
		"phases":   bootTimer.PhaseMap(),
	}).Info("Thaw agent initialization complete")

	// After snapshot restore, this process resumes from here (not from main()).
	// The host sets new MMDS data (including current_time) after restore.
	// Poll MMDS for clock sync and re-run initialization if we detect a restore.
	go watchForSnapshotRestore()

	// Block forever - health server runs in background, runner runs as separate process
	select {}
}

func resolveDevice(defaultDev string, label string, fallbackLabels ...string) string {
	// Prefer by-label path if present (try primary label first, then fallbacks).
	byLabel := filepath.Join("/dev/disk/by-label", label)
	if _, err := os.Stat(byLabel); err == nil {
		return byLabel
	}
	for _, fb := range fallbackLabels {
		byLabel = filepath.Join("/dev/disk/by-label", fb)
		if _, err := os.Stat(byLabel); err == nil {
			return byLabel
		}
	}
	// Fall back to default device path.
	return defaultDev
}

// mountExtensionDrives mounts all extension drives declared in MMDS.
func mountExtensionDrives(data *MMDSData) {
	for _, drive := range data.Latest.Drives {
		if drive.MountPath == "" {
			continue
		}
		label := drive.Label
		if label == "" {
			label = strings.ReplaceAll(strings.ToUpper(drive.DriveID), "-", "_")
		}
		dev := resolveDevice("/dev/disk/by-label/"+label, label)

		if err := os.MkdirAll(drive.MountPath, 0755); err != nil {
			log.WithError(err).WithField("drive", drive.DriveID).Warn("Failed to create mount dir")
			continue
		}
		// Skip if already mounted
		if exec.Command("mountpoint", "-q", drive.MountPath).Run() == nil {
			continue
		}
		opts := "ro"
		if !drive.ReadOnly {
			opts = "rw"
		}
		if output, err := exec.Command("mount", "-o", opts, dev, drive.MountPath).CombinedOutput(); err != nil {
			log.WithError(err).WithFields(logrus.Fields{
				"drive":  drive.DriveID,
				"device": dev,
				"mount":  drive.MountPath,
				"output": strings.TrimSpace(string(output)),
			}).Warn("Failed to mount extension drive")
		} else {
			log.WithFields(logrus.Fields{
				"drive": drive.DriveID,
				"mount": drive.MountPath,
			}).Info("Mounted extension drive")
		}
	}
}

func waitForMMDS(ctx context.Context) (*MMDSData, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, "GET", *mmdsEndpoint+"/latest", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.WithError(err).Debug("MMDS not ready, retrying...")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		var data MMDSData
		// MMDS can return data wrapped in "latest" key OR directly
		json.Unmarshal(body, &data)

		// If the "latest" wrapper wasn't present, try parsing directly as inner structure
		if data.Latest.Meta.RunnerID == "" {
			var inner struct {
				Meta struct {
					RunnerID     string `json:"runner_id"`
					HostID       string `json:"host_id"`
					InstanceName string `json:"instance_name,omitempty"`
					Environment  string `json:"environment"`
					JobID        string `json:"job_id,omitempty"`
					Mode         string `json:"mode,omitempty"`
					CurrentTime  string `json:"current_time,omitempty"`
				} `json:"meta"`
				Network struct {
					IP        string `json:"ip"`
					Gateway   string `json:"gateway"`
					Netmask   string `json:"netmask"`
					DNS       string `json:"dns"`
					Interface string `json:"interface"`
					MAC       string `json:"mac"`
				} `json:"network"`
				Job struct {
					Repo          string            `json:"repo"`
					Branch        string            `json:"branch"`
					Commit        string            `json:"commit"`
					GitToken      string            `json:"git_token"`
					Labels        map[string]string `json:"labels"`
				} `json:"job"`
				Snapshot struct {
					Version string `json:"version"`
				} `json:"snapshot"`
				Drives []snapshot.DriveSpec `json:"drives,omitempty"`
				Exec   struct {
					Command    []string          `json:"command,omitempty"`
					Env        map[string]string `json:"env,omitempty"`
					WorkingDir string            `json:"working_dir,omitempty"`
					TimeoutSec int               `json:"timeout_seconds,omitempty"`
				} `json:"exec,omitempty"`
				Warmup struct {
					Commands []snapshot.SnapshotCommand `json:"commands,omitempty"`
				} `json:"warmup,omitempty"`
				// Mirrors snapshot.StartCommand — keep in sync with pkg/snapshot/start_command.go.
				StartCommand struct {
					Command    []string          `json:"command,omitempty"`
					Port       int               `json:"port,omitempty"`
					HealthPath string            `json:"health_path,omitempty"`
					Env        map[string]string `json:"env,omitempty"`
					RunAs      string            `json:"run_as,omitempty"`
				} `json:"start_command,omitempty"`
			}
			if err := json.Unmarshal(body, &inner); err != nil {
				return nil, fmt.Errorf("failed to parse MMDS data: %w", err)
			}
			data.Latest = inner
		}

		// Wait until runner_id is populated - manager sets MMDS after VM boots
		if data.Latest.Meta.RunnerID == "" {
			log.Debug("MMDS data not fully populated yet (no runner_id), retrying...")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		return &data, nil
	}
}

// fetchMMDSData does a single non-blocking fetch of MMDS data.
// Unlike waitForMMDS, it doesn't retry or wait for runner_id.
// Used for polling MMDS after snapshot restore.
func fetchMMDSData() (*MMDSData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Reuse waitForMMDS logic but with a short timeout
	// This handles all the JSON parsing complexity
	return waitForMMDSOnce(ctx)
}

// waitForMMDSOnce does a single attempt to fetch and parse MMDS data.
func waitForMMDSOnce(ctx context.Context) (*MMDSData, error) {
	client := &http.Client{Timeout: 2 * time.Second}

	req, err := http.NewRequestWithContext(ctx, "GET", *mmdsEndpoint+"/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MMDS returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	log.WithFields(logrus.Fields{
		"body_size":    len(body),
		"body_preview": string(body[:min(len(body), 500)]),
	}).Debug("Raw MMDS response")

	var data MMDSData
	if err := json.Unmarshal(body, &data); err != nil {
		log.WithError(err).Debug("Failed to parse MMDS into MMDSData wrapper")
		return nil, fmt.Errorf("failed to parse MMDS: %w", err)
	}

	log.WithFields(logrus.Fields{
		"runner_id":       data.Latest.Meta.RunnerID,
		"mode":            data.Latest.Meta.Mode,
		"job_repo":        data.Latest.Job.Repo,
		"has_git_token":   data.Latest.Job.GitToken != "",
		"git_token_len":   len(data.Latest.Job.GitToken),
		"warmup_commands": len(data.Latest.Warmup.Commands),
	}).Debug("Parsed MMDS data (first pass)")

	// Handle unwrapped format - try to parse directly into Latest
	if data.Latest.Meta.RunnerID == "" && len(body) > 0 {
		log.Debug("RunnerID empty after first parse, trying unwrapped format")
		if err := json.Unmarshal(body, &data.Latest); err == nil {
			log.WithFields(logrus.Fields{
				"runner_id":       data.Latest.Meta.RunnerID,
				"mode":            data.Latest.Meta.Mode,
				"job_repo":        data.Latest.Job.Repo,
				"has_git_token":   data.Latest.Job.GitToken != "",
				"git_token_len":   len(data.Latest.Job.GitToken),
				"warmup_commands": len(data.Latest.Warmup.Commands),
			}).Debug("Parsed MMDS data (unwrapped format)")
		} else {
			log.WithError(err).Debug("Failed to parse MMDS in unwrapped format")
		}
	}

	return &data, nil
}

func configureNetwork(data *MMDSData) error {
	net := data.Latest.Network
	if net.IP == "" {
		return fmt.Errorf("no IP address in MMDS data")
	}

	iface := net.Interface
	if iface == "" {
		iface = "eth0"
	}

	// Set MAC address if provided. Snapshot-restored VMs all share the same
	// MAC baked into the snapshot. Each slot gets a unique MAC via MMDS to
	// avoid bridge forwarding table collisions when multiple VMs run on the
	// same host.
	if net.MAC != "" {
		if err := exec.Command("ip", "link", "set", iface, "address", net.MAC).Run(); err != nil {
			log.WithError(err).WithField("mac", net.MAC).Warn("Failed to set MAC address")
		} else {
			log.WithField("mac", net.MAC).Info("MAC address configured")
		}
	}

	// Set MTU to avoid fragmentation on GCP (ens4 MTU=1460).
	// The guest eth0 defaults to 1500 but the host TAP/bridge/veth path
	// is clamped to the external interface MTU. Read the current MTU from
	// sysfs and only adjust if it's already lower (set by the TAP).
	mtuBytes, mtuErr := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/mtu", iface))
	if mtuErr == nil {
		currentMTU := strings.TrimSpace(string(mtuBytes))
		if currentMTU == "1500" {
			// Try setting to 1460 (GCP standard). If the TAP supports it, great.
			// If not, the command will fail silently and we keep 1500.
			if err := exec.Command("ip", "link", "set", iface, "mtu", "1460").Run(); err == nil {
				log.Info("Guest interface MTU set to 1460")
			}
		}
	}

	// Fix nsswitch.conf: remove systemd-resolve references so glibc uses /etc/resolv.conf directly.
	// Ubuntu 24.04 ships "hosts: files resolve [!UNAVAIL=return] dns" which breaks DNS
	// when systemd-resolved is masked (as in Firecracker VMs).
	if nssData, err := os.ReadFile("/etc/nsswitch.conf"); err == nil {
		nss := string(nssData)
		if strings.Contains(nss, "resolve") {
			nss = strings.ReplaceAll(nss, "resolve [!UNAVAIL=return]", "")
			nss = strings.ReplaceAll(nss, "resolve", "")
			os.WriteFile("/etc/nsswitch.conf", []byte(nss), 0644)
			log.Info("Fixed nsswitch.conf: removed systemd-resolve references")
		}
	}

	// Check if kernel already configured the network (via ip= boot parameter)
	// Use Go's net package since the guest rootfs may not have the `ip` command.
	expectedIP := strings.Split(net.IP, "/")[0]
	ipAlreadyConfigured := false
	if goIface, err := gonet.InterfaceByName(iface); err == nil {
		addrs, _ := goIface.Addrs()
		for _, addr := range addrs {
			if strings.Contains(addr.String(), expectedIP) {
				ipAlreadyConfigured = true
				break
			}
		}
		log.WithFields(logrus.Fields{"iface": iface, "addrs": fmt.Sprintf("%v", addrs), "found": ipAlreadyConfigured}).Info("configureNetwork: checked interface via Go net package")
	} else {
		log.WithError(err).Warn("configureNetwork: could not get interface by name, falling back to ip command")
		// Fallback to ip command
		out, _ := exec.Command("ip", "addr", "show", "dev", iface).CombinedOutput()
		ipAlreadyConfigured = strings.Contains(string(out), expectedIP)
	}
	if ipAlreadyConfigured {
		log.WithField("ip", expectedIP).Info("Network IP already configured by kernel, ensuring DNS and routes are set")

		// Ensure interface is up (check via /sys/class/net, no ip command needed)
		if operState, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/operstate", iface)); err == nil {
			log.WithField("operstate", strings.TrimSpace(string(operState))).Info("configureNetwork: interface operstate")
			if strings.TrimSpace(string(operState)) != "up" {
				// Try ip command if available, otherwise interface should come up on its own
				exec.Command("ip", "link", "set", iface, "up").Run()
			}
		}

		// Check default route via /proc/net/route (no ip command needed)
		// Format: Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
		// Default route has Destination=00000000
		if net.Gateway != "" {
			routeData, routeErr := os.ReadFile("/proc/net/route")
			log.WithFields(logrus.Fields{"routes": string(routeData), "err": routeErr}).Info("configureNetwork: /proc/net/route")
			hasDefaultRoute := false
			if routeErr == nil {
				for _, line := range strings.Split(string(routeData), "\n") {
					fields := strings.Fields(line)
					if len(fields) >= 3 && fields[1] == "00000000" {
						hasDefaultRoute = true
						break
					}
				}
			}
			if hasDefaultRoute {
				log.Info("configureNetwork: default route already exists")
			} else {
				log.WithField("gateway", net.Gateway).Warn("configureNetwork: no default route found, trying to add via ip command")
				exec.Command("ip", "route", "add", "default", "via", net.Gateway).Run()
			}
		} else {
			log.Warn("configureNetwork: no gateway in MMDS data!")
		}

		// Configure DNS since kernel ip= parameter doesn't set it
		if net.DNS != "" {
			// Remove symlink if present (Ubuntu uses symlink to systemd-resolved stub)
			if linkTarget, err := os.Readlink("/etc/resolv.conf"); err == nil {
				log.WithField("target", linkTarget).Info("configureNetwork: resolv.conf is symlink, removing")
				os.Remove("/etc/resolv.conf")
			}
			resolv := fmt.Sprintf("nameserver %s\n", net.DNS)
			if err := os.WriteFile("/etc/resolv.conf", []byte(resolv), 0644); err != nil {
				log.WithError(err).Warn("Failed to write resolv.conf")
			} else {
				log.WithField("dns", net.DNS).Info("configureNetwork: wrote resolv.conf")
			}
		} else {
			log.Warn("configureNetwork: no DNS in MMDS data!")
		}

		// Verify after config
		resolvCheck, _ := os.ReadFile("/etc/resolv.conf")
		routeCheck, _ := os.ReadFile("/proc/net/route")
		log.WithFields(logrus.Fields{
			"resolv_conf": strings.TrimSpace(string(resolvCheck)),
			"routes":      strings.TrimSpace(string(routeCheck)),
		}).Info("configureNetwork: post-config state")

		// Quick connectivity test using Go's net.DialTimeout (no ping needed)
		testConn, testErr := gonet.DialTimeout("tcp", net.Gateway+":80", 2*time.Second)
		if testErr != nil {
			// TCP to gateway:80 won't work, try UDP DNS instead
			log.WithField("gateway_test_err", testErr.Error()).Info("configureNetwork: gateway TCP test (expected to fail, not a real service)")
		} else {
			testConn.Close()
			log.Info("configureNetwork: gateway reachable")
		}

		return nil
	}

	// Only configure if kernel didn't set it up
	log.Info("Configuring network from MMDS data...")

	// Flush existing addresses
	exec.Command("ip", "addr", "flush", "dev", iface).Run()

	// Add IP address
	if err := exec.Command("ip", "addr", "add", net.IP, "dev", iface).Run(); err != nil {
		return fmt.Errorf("failed to add IP address: %w", err)
	}

	// Bring interface up
	if err := exec.Command("ip", "link", "set", iface, "up").Run(); err != nil {
		return fmt.Errorf("failed to bring interface up: %w", err)
	}

	// Add default route
	if net.Gateway != "" {
		exec.Command("ip", "route", "del", "default").Run()
		if err := exec.Command("ip", "route", "add", "default", "via", net.Gateway).Run(); err != nil {
			return fmt.Errorf("failed to add default route: %w", err)
		}
	}

	// Configure DNS
	if net.DNS != "" {
		resolv := fmt.Sprintf("nameserver %s\n", net.DNS)
		if err := os.WriteFile("/etc/resolv.conf", []byte(resolv), 0644); err != nil {
			return fmt.Errorf("failed to write resolv.conf: %w", err)
		}
	}

	log.WithFields(logrus.Fields{
		"interface": iface,
		"ip":        net.IP,
		"gateway":   net.Gateway,
		"dns":       net.DNS,
		"mac":       net.MAC,
	}).Info("Network configured")

	return nil
}

func regenerateHostname(runnerID string) error {
	// Handle empty or short runner IDs gracefully
	shortID := runnerID
	if len(shortID) > 8 {
		shortID = runnerID[:8]
	}
	if shortID == "" {
		shortID = "unknown"
	}
	hostname := fmt.Sprintf("runner-%s", shortID)

	if err := os.WriteFile("/etc/hostname", []byte(hostname+"\n"), 0644); err != nil {
		return err
	}

	return exec.Command("hostname", hostname).Run()
}

// watchForSnapshotRestore polls MMDS for a current_time field and syncs the
// guest clock and network when it changes. After a diff snapshot resume, the
// thaw-agent process continues from where it was paused (at select{} in main).
// The host manager sets new MMDS data (including current_time and network
// config) after restore. This goroutine detects that, syncs the clock, and
// reconfigures the guest network to match the new TAP slot.
func watchForSnapshotRestore() {
	endpoint := *mmdsEndpoint + "/latest/meta/current_time"
	lastTime := ""

	for {
		time.Sleep(1 * time.Second)

		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")

		// Use a fresh client per request — after snapshot restore, pooled
		// connections in http.DefaultClient are stale/dead.
		client := &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			continue
		}

		// MMDS V1 returns the value as a JSON string (quoted) or plain text
		ct := strings.TrimSpace(string(body))
		ct = strings.Trim(ct, "\"") // Remove JSON quotes if present
		if ct == "" || ct == lastTime {
			continue
		}

		// New current_time detected — this means we were restored from a snapshot
		lastTime = ct
		hostTime, err := time.Parse(time.RFC3339, ct)
		if err != nil {
			log.WithError(err).Warn("watchForSnapshotRestore: failed to parse current_time")
			continue
		}

		tv := syscall.Timeval{
			Sec:  hostTime.Unix(),
			Usec: 0,
		}
		if err := syscall.Settimeofday(&tv); err != nil {
			// Fallback: use date command
			formatted := hostTime.UTC().Format("2006-01-02 15:04:05")
			if output, err := exec.Command("date", "-u", "-s", formatted).CombinedOutput(); err != nil {
				log.WithError(err).WithField("output", string(output)).Warn("watchForSnapshotRestore: date command failed")
				continue
			}
		}

		log.WithFields(logrus.Fields{
			"source":      "mmds-watcher",
			"server_time": hostTime.UTC().Format(time.RFC3339),
		}).Info("Clock synced from MMDS after snapshot restore")

		// Reconfigure network — the VM may have been resumed on a different
		// TAP slot with a new IP. Without this, the guest retains the old IP
		// and cannot receive traffic.
		newData, fetchErr := fetchMMDSData()
		if fetchErr != nil {
			log.WithError(fetchErr).Warn("watchForSnapshotRestore: failed to fetch MMDS for network reconfig")
			continue
		}
		if !*skipNetwork && newData.Latest.Network.IP != "" {
			if err := configureNetwork(newData); err != nil {
				log.WithError(err).Warn("watchForSnapshotRestore: network reconfiguration failed")
			} else {
				log.WithField("new_ip", newData.Latest.Network.IP).Info("Network reconfigured after snapshot restore")
			}
		}
	}
}

func resyncClock(mmdsData *MMDSData) error {
	// After snapshot restore, the guest clock is stuck at snapshot creation time.
	// We need to set it to current time before GitHub runner registration (which
	// rejects clocks skewed >5 minutes).

	// Method 1: Use the host timestamp from MMDS (most reliable, no network needed).
	// The host manager sets meta.current_time when building MMDS data.
	if mmdsData != nil && mmdsData.Latest.Meta.CurrentTime != "" {
		hostTime, err := time.Parse(time.RFC3339, mmdsData.Latest.Meta.CurrentTime)
		if err == nil {
			tv := syscall.Timeval{
				Sec:  hostTime.Unix(),
				Usec: 0,
			}
			if err := syscall.Settimeofday(&tv); err == nil {
				log.WithFields(logrus.Fields{
					"source":      "mmds",
					"server_time": hostTime.UTC().Format(time.RFC3339),
				}).Info("Clock synced from MMDS host timestamp")
				return nil
			}
			// Fallback: use date command
			formatted := hostTime.UTC().Format("2006-01-02 15:04:05")
			if output, err := exec.Command("date", "-u", "-s", formatted).CombinedOutput(); err == nil {
				log.WithFields(logrus.Fields{
					"source":      "mmds+date",
					"server_time": hostTime.UTC().Format(time.RFC3339),
				}).Info("Clock synced from MMDS host timestamp via date command")
				return nil
			} else {
				log.WithError(err).WithField("output", string(output)).Warn("Clock sync: date command failed for MMDS time")
			}
		} else {
			log.WithError(err).WithField("time_str", mmdsData.Latest.Meta.CurrentTime).Warn("Clock sync: failed to parse MMDS timestamp")
		}
	}

	// Method 2: Fetch time from an HTTP server via Date header.
	// IMPORTANT: Use plain HTTP (not HTTPS) because clock skew breaks TLS.
	// Retry multiple times because network may not be fully ready.
	sources := []string{
		"http://connectivitycheck.gstatic.com/generate_204",
		"http://www.gstatic.com/generate_204",
		"http://www.google.com",
	}

	// Disable redirects so we get the Date header from the first response
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Retry up to 5 times with increasing delays, because the network
	// stack may need time to settle after snapshot restore + reconfiguration.
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * time.Second
			log.WithFields(logrus.Fields{
				"attempt": attempt + 1,
				"delay":   delay,
			}).Debug("Clock sync: retrying after delay")
			time.Sleep(delay)
		}

		for _, url := range sources {
			req, err := http.NewRequest("HEAD", url, nil)
			if err != nil {
				continue
			}

			resp, err := client.Do(req)
			if err != nil {
				log.WithError(err).WithField("url", url).Debug("Clock sync: failed to reach server")
				continue
			}
			resp.Body.Close()

			dateStr := resp.Header.Get("Date")
			if dateStr == "" {
				log.WithField("url", url).Debug("Clock sync: no Date header")
				continue
			}

			// Parse HTTP date (RFC1123 format: "Sat, 14 Feb 2026 15:00:00 GMT")
			serverTime, err := http.ParseTime(dateStr)
			if err != nil {
				log.WithError(err).WithField("date", dateStr).Debug("Clock sync: failed to parse Date header")
				continue
			}

			// Set system clock
			tv := syscall.Timeval{
				Sec:  serverTime.Unix(),
				Usec: 0,
			}
			if err := syscall.Settimeofday(&tv); err != nil {
				log.WithError(err).Debug("Clock sync: settimeofday failed, trying date command")
				// Fallback: use date command
				formatted := serverTime.UTC().Format("2006-01-02 15:04:05")
				if output, err := exec.Command("date", "-u", "-s", formatted).CombinedOutput(); err != nil {
					log.WithError(err).WithField("output", string(output)).Warn("Clock sync: date command failed")
					continue
				}
			}

			log.WithFields(logrus.Fields{
				"source":      url,
				"server_time": serverTime.UTC().Format(time.RFC3339),
				"attempt":     attempt + 1,
			}).Info("Clock synced from HTTP Date header")
			return nil
		}
	}

	// Last resort: try ntpdate if available
	if output, err := exec.Command("ntpdate", "-u", "pool.ntp.org").CombinedOutput(); err == nil {
		log.Info("Clock synced via ntpdate")
		return nil
	} else {
		log.WithError(err).WithField("output", string(output)).Debug("ntpdate fallback failed")
	}

	return fmt.Errorf("failed to sync clock from any source")
}

func signalReady() error {
	readyDir := filepath.Dir(*readyFile)
	if err := os.MkdirAll(readyDir, 0755); err != nil {
		return err
	}

	return os.WriteFile(*readyFile, []byte(time.Now().Format(time.RFC3339)), 0644)
}

// runWarmupMode runs the snapshot warmup process by dispatching each command,
// then runs infra-level finalization steps (runner update, page pre-warm) that
// are always needed regardless of which user commands were specified.
func runWarmupMode(data *MMDSData) error {
	for _, cmd := range data.Latest.Warmup.Commands {
		if err := dispatchCommand(cmd, data); err != nil {
			return fmt.Errorf("command %s failed: %w", cmd.Type, err)
		}
	}

	updateWarmupState("syncing", "Syncing caches to disk...")
	exec.Command("sync").Run()

	return nil
}

// dispatchCommand routes a SnapshotCommand to its handler.
func dispatchCommand(cmd snapshot.SnapshotCommand, data *MMDSData) error {
	switch cmd.Type {
	case "shell":
		return runShellCommand(cmd.Args, cmd.RunAsRoot, data)
	case "base-image", "platform-setup", "platform-user":
		// Declarative markers used for layer hash computation only;
		// the snapshot-builder handles these during rootfs setup before VM boot.
		return nil
	default:
		return fmt.Errorf("unknown command type: %q", cmd.Type)
	}
}

// runShellCommand runs an arbitrary shell command with MMDS credentials
// injected as environment variables so callers can use $GIT_TOKEN, etc.
// without any extra setup steps.
// When runAsRoot is false the command is run as the configured runner user.
func runShellCommand(args []string, runAsRoot bool, data *MMDSData) error {
	if len(args) == 0 {
		return fmt.Errorf("shell command requires at least one argument")
	}
	env := os.Environ()
	if data != nil {
		if t := data.Latest.Job.GitToken; t != "" {
			env = append(env, "GIT_TOKEN="+t)
		}
	}
	var procAttr *syscall.SysProcAttr
	if !runAsRoot {
		runnerUser, err := user.Lookup(*runnerUsername)
		if err != nil {
			return fmt.Errorf("runner user %q not found: %w", *runnerUsername, err)
		}
		rUID, _ := strconv.ParseUint(runnerUser.Uid, 10, 32)
		rGID, _ := strconv.ParseUint(runnerUser.Gid, 10, 32)
		procAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(rUID), Gid: uint32(rGID)},
		}
		env = append(env, "HOME="+runnerUser.HomeDir)
	}
	globalWarmupLogs.Add(fmt.Sprintf("[shell] Running: %s", strings.Join(args, " ")))
	if err := runStreamedCommand("", env, procAttr, args[0], args[1:]...); err != nil {
		globalWarmupLogs.Add(fmt.Sprintf("[shell] FAILED: %s: %v", strings.Join(args, " "), err))
		return err
	}
	return nil
}

// runStreamedCommand runs a command, capturing stdout/stderr line by line
// into the warmup log buffer and the structured logger.
func runStreamedCommand(dir string, env []string, procAttr *syscall.SysProcAttr, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if procAttr != nil {
		cmd.SysProcAttr = procAttr
	}

	// Create pipes for stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Stream both stdout and stderr concurrently
	var wg sync.WaitGroup
	streamLines := func(prefix string, r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 256*1024), 256*1024) // 256KB line buffer for bazel
		for scanner.Scan() {
			line := scanner.Text()
			log.WithField("src", prefix).Debug(line)
			globalWarmupLogs.Add(fmt.Sprintf("[%s] %s", prefix, line))
		}
	}

	wg.Add(2)
	go streamLines("stdout", stdoutPipe)
	go streamLines("stderr", stderrPipe)
	wg.Wait()

	return cmd.Wait()
}

func updateWarmupState(phase, message string) {
	globalWarmupState.Phase = phase
	globalWarmupState.Message = message
	log.WithFields(logrus.Fields{
		"phase":   phase,
		"message": message,
	}).Info("Warmup progress")
}

// execHandler handles POST /exec requests — runs a command inside the VM and
// streams stdout/stderr/exit as ndjson frames.
func execHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Command    []string          `json:"command"`
		Env        map[string]string `json:"env"`
		WorkingDir string            `json:"working_dir"`
		TimeoutSec int               `json:"timeout_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Command) == 0 {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	// Set streaming headers
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Build context with optional timeout
	ctx := r.Context()
	if req.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSec)*time.Second)
		defer cancel()
	}

	// Build command
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)

	// Working directory
	cmd.Dir = req.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = *workspaceDir
	}

	// Environment: inherit current env + merge provided env (deduped)
	cmd.Env = mergeEnv(os.Environ(), req.Env)

	// Run as runner user
	runnerUser, err := user.Lookup(*runnerUsername)
	if err != nil {
		writeNDJSON(w, flusher, map[string]interface{}{"type": "error", "message": "runner user not found: " + err.Error(), "ts": time.Now().UTC().Format(time.RFC3339Nano)})
		return
	}
	rUID, _ := strconv.ParseUint(runnerUser.Uid, 10, 32)
	rGID, _ := strconv.ParseUint(runnerUser.Gid, 10, 32)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(rUID), Gid: uint32(rGID)},
	}
	cgroupMgr.applyCgroup(cmd.SysProcAttr)
	cmd.Env = append(cmd.Env, "HOME="+runnerUser.HomeDir)

	// Create pipes
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		writeNDJSON(w, flusher, map[string]interface{}{"type": "error", "message": "stdout pipe: " + err.Error(), "ts": time.Now().UTC().Format(time.RFC3339Nano)})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		writeNDJSON(w, flusher, map[string]interface{}{"type": "error", "message": "stderr pipe: " + err.Error(), "ts": time.Now().UTC().Format(time.RFC3339Nano)})
		return
	}

	// Start command
	if err := cmd.Start(); err != nil {
		writeNDJSON(w, flusher, map[string]interface{}{"type": "error", "message": "start failed: " + err.Error(), "ts": time.Now().UTC().Format(time.RFC3339Nano)})
		return
	}

	// Stream stdout/stderr concurrently
	var wg sync.WaitGroup
	var mu sync.Mutex

	streamPipe := func(streamType string, pipe io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(pipe)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		for scanner.Scan() {
			mu.Lock()
			writeNDJSON(w, flusher, map[string]interface{}{
				"type": streamType,
				"data": scanner.Text() + "\n",
				"ts":   time.Now().UTC().Format(time.RFC3339Nano),
			})
			mu.Unlock()
		}
	}

	wg.Add(2)
	go streamPipe("stdout", stdoutPipe)
	go streamPipe("stderr", stderrPipe)
	wg.Wait()

	// Wait for command to finish
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	// Write timeout error frame if context was cancelled
	if ctx.Err() == context.DeadlineExceeded {
		writeNDJSON(w, flusher, map[string]interface{}{"type": "error", "message": "timeout", "ts": time.Now().UTC().Format(time.RFC3339Nano)})
	}

	// Write exit frame
	writeNDJSON(w, flusher, map[string]interface{}{"type": "exit", "code": exitCode, "ts": time.Now().UTC().Format(time.RFC3339Nano)})
}

// writeNDJSON writes a single ndjson frame and flushes.
func writeNDJSON(w http.ResponseWriter, flusher http.Flusher, data map[string]interface{}) {
	b, _ := json.Marshal(data)
	w.Write(b)
	w.Write([]byte("\n"))
	flusher.Flush()
}

const serviceLogPath = "/var/log/start-command.log"

// runStartCommand starts the user's service process in the background and polls
// its health endpoint until it returns 200 or the timeout expires.
func runStartCommand(mmdsData *MMDSData) error {
	sc := mmdsData.Latest.StartCommand
	if len(sc.Command) == 0 {
		return nil
	}

	log.WithFields(logrus.Fields{
		"command":     sc.Command,
		"port":        sc.Port,
		"health_path": sc.HealthPath,
	}).Info("Starting user service via start_command")

	cmd := exec.Command(sc.Command[0], sc.Command[1:]...)
	cmd.Dir = *workspaceDir

	// Set environment: inherit current process env + merge start_command overrides
	cmd.Env = mergeEnv(os.Environ(), sc.Env)
	if len(sc.Env) > 0 {
		log.WithField("env_count", len(sc.Env)).Info("Injected start_command env vars")
	}

	// Run as the specified user (or default to --runner-user flag)
	runAsUser := sc.RunAs
	if runAsUser == "" {
		runAsUser = *runnerUsername
	}
	targetUser, err := user.Lookup(runAsUser)
	if err != nil {
		return fmt.Errorf("run_as user %q not found: %w", runAsUser, err)
	}
	rUID, _ := strconv.ParseUint(targetUser.Uid, 10, 32)
	rGID, _ := strconv.ParseUint(targetUser.Gid, 10, 32)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(rUID), Gid: uint32(rGID)},
		Setsid:     true,
	}
	cgroupMgr.applyCgroup(cmd.SysProcAttr)
	cmd.Env = append(cmd.Env, "HOME="+targetUser.HomeDir)

	log.WithFields(logrus.Fields{
		"run_as": runAsUser,
		"uid":    rUID,
		"gid":    rGID,
	}).Info("start_command will run as user")

	// Open log file for stdout+stderr
	logFile, err := os.OpenFile(serviceLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open service log file: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start service: %w", err)
	}

	log.WithField("pid", cmd.Process.Pid).Info("Service process started")

	// Monitor for early exit in background
	go func() {
		err := cmd.Wait()
		logFile.Close()
		if err != nil {
			log.WithError(err).Error("Service process exited with error")
		} else {
			log.Info("Service process exited cleanly")
		}
	}()

	// Poll health endpoint if configured
	if sc.Port > 0 && sc.HealthPath != "" {
		healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", sc.Port, sc.HealthPath)
		client := &http.Client{Timeout: 2 * time.Second}

		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			resp, err := client.Get(healthURL)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					log.WithField("health_url", healthURL).Info("Service health check passed")
					return nil
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		return fmt.Errorf("service health check timed out after 2 minutes: %s", healthURL)
	}

	return nil
}

// serviceLogsHandler serves the service log file on the debug port.
// GET /service-logs — returns full log content
// GET /service-logs?follow=true — streams new lines as they appear (chunked transfer encoding)
func serviceLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("follow") == "true" {
		// Streaming mode
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)

		f, err := os.Open(serviceLogPath)
		if err != nil {
			fmt.Fprintf(w, "no service logs yet: %v\n", err)
			flusher.Flush()
			return
		}
		defer f.Close()

		// Seek to end and stream new lines
		f.Seek(0, io.SeekEnd)
		scanner := bufio.NewScanner(f)
		for {
			for scanner.Scan() {
				fmt.Fprintln(w, scanner.Text())
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
				// Continue polling for new lines
			}
		}
	}

	// Non-streaming: return full file
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	data, err := os.ReadFile(serviceLogPath)
	if err != nil {
		http.Error(w, "no service logs available", http.StatusNotFound)
		return
	}
	w.Write(data)
}

// startHealthServer starts a simple HTTP server for health checks and testing
func startHealthServer(mmdsData *MMDSData) {
	defer func() {
		if r := recover(); r != nil {
			log.WithField("panic", r).Error("Health server panicked!")
		}
	}()

	log.Info("Creating health server on :10500...")

	// Use a separate ServeMux to avoid conflicts with the default mux (used by :10501)
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Safely access MMDS data
		runnerID := ""
		mode := ""
		if mmdsData != nil {
			runnerID = mmdsData.Latest.Meta.RunnerID
			mode = mmdsData.Latest.Meta.Mode
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":       "healthy",
			"runner_id":    runnerID,
			"mode":         mode,
			"uptime":       time.Since(globalWarmupState.StartedAt).String(),
		})
	})

	// Warmup status endpoint (for snapshot-builder to poll)
	mux.HandleFunc("/warmup-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(globalWarmupState)
	})

	// MMDS diagnostic endpoint - queries MMDS directly from inside VM
	mux.HandleFunc("/mmds-diag", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Query MMDS directly
		client := &http.Client{Timeout: 2 * time.Second}
		req, _ := http.NewRequest("GET", *mmdsEndpoint+"/latest", nil)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)

		var mmdsRaw string
		var mmdsErr string
		if err != nil {
			mmdsErr = err.Error()
		} else {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			mmdsRaw = string(body)
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"mmds_endpoint":     *mmdsEndpoint,
			"mmds_raw":          mmdsRaw,
			"mmds_error":        mmdsErr,
			"current_runner_id": mmdsData.Latest.Meta.RunnerID,
			"current_mode":      mmdsData.Latest.Meta.Mode,
		})
	})

	// Warmup log streaming endpoint - returns new log lines since ?after=N
	mux.HandleFunc("/warmup-logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		afterSeq := int64(0)
		if s := r.URL.Query().Get("after"); s != "" {
			afterSeq, _ = strconv.ParseInt(s, 10, 64)
		}
		lines, seq := globalWarmupLogs.Since(afterSeq)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"lines": lines,
			"seq":   seq,
		})
	})

	// Network info endpoint
	mux.HandleFunc("/network", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Get actual network config
		out, _ := exec.Command("ip", "addr", "show", "eth0").Output()
		route, _ := exec.Command("ip", "route").Output()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"configured_ip": mmdsData.Latest.Network.IP,
			"gateway":       mmdsData.Latest.Network.Gateway,
			"ip_addr":       string(out),
			"routes":        string(route),
		})
	})

	// Test internet connectivity
	mux.HandleFunc("/connectivity", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pingOut, pingErr := exec.Command("ping", "-c", "1", "-W", "2", "8.8.8.8").CombinedOutput()
		dnsOut, dnsErr := exec.Command("ping", "-c", "1", "-W", "2", "google.com").CombinedOutput()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ping_8888":   pingErr == nil,
			"ping_output": string(pingOut),
			"dns_works":   dnsErr == nil,
			"dns_output":  string(dnsOut),
		})
	})

	// Debug endpoint for mounts and block devices
	mux.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mounts, _ := exec.Command("mount").Output()
		lsblk, _ := exec.Command("lsblk", "-o", "NAME,SIZE,TYPE,MOUNTPOINT").Output()
		df, _ := exec.Command("df", "-h").Output()
		runnerCheck, _ := exec.Command("ls", "-la", "/home/runner").CombinedOutput()
		workspaceLs, _ := exec.Command("ls", "-la", *workspaceDir).CombinedOutput()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"mounts":       string(mounts),
			"lsblk":        string(lsblk),
			"df":           string(df),
			"runner_dir":   string(runnerCheck),
			"workspace_ls": string(workspaceLs),
		})
	})

	server := &http.Server{
		Addr:    ":10500",
		Handler: mux,
	}

	// Check if :10500 is already bound (from warmup mode). If so, skip —
	// the warmup health server is already running and has the same endpoints.
	ln, err := gonet.Listen("tcp", ":10500")
	if err != nil {
		log.Info("Health server :10500 already running (from warmup), skipping")
		return
	}
	ln.Close()

	log.Info("Attempting to start health server on :10500...")
	if err := server.ListenAndServe(); err != nil {
		log.WithError(err).Error("Health server on :10500 failed to start or stopped")
	} else {
		log.Info("Health server on :10500 stopped gracefully")
	}
}

// loadDockerEnv reads /etc/environment and sets any variables not already
// present in the current process environment. The snapshot-builder writes
// Docker image ENV directives here during rootfs creation. This ensures
// child processes (start_command, shell warmup commands, etc.) inherit
// PATH, PYTHONPATH, and other vars the Docker image defined.
func loadDockerEnv() {
	f, err := os.Open("/etc/environment")
	if err != nil {
		return // file doesn't exist on non-Docker-based images
	}
	defer f.Close()

	loaded := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		key := line[:idx]
		val := line[idx+1:]
		if key == "PATH" {
			// Merge Docker PATH with existing PATH: prepend Docker-specific
			// directories that aren't already present (e.g., /opt/venv/bin).
			existing := os.Getenv("PATH")
			existingDirs := make(map[string]bool)
			for _, d := range strings.Split(existing, ":") {
				existingDirs[d] = true
			}
			var newDirs []string
			for _, d := range strings.Split(val, ":") {
				if !existingDirs[d] {
					newDirs = append(newDirs, d)
				}
			}
			if len(newDirs) > 0 {
				merged := strings.Join(newDirs, ":") + ":" + existing
				os.Setenv("PATH", merged)
				loaded++
			}
		} else if os.Getenv(key) == "" {
			// Don't override vars already set (e.g., by systemd unit Environment=)
			os.Setenv(key, val)
			loaded++
		}
	}
	if loaded > 0 && log != nil {
		log.WithField("count", loaded).Info("Loaded Docker ENV from /etc/environment")
	}
}

// mergeEnv merges override env vars into a base env slice (as returned by
// os.Environ()). For PATH, override directories not already present are
// appended. For all other variables, the override replaces any existing value.
// Returns a clean, deduplicated slice.
func mergeEnv(baseEnv []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return baseEnv
	}

	// Parse base into ordered key→value map, preserving insertion order via keys slice.
	type entry struct {
		key string
		val string
	}
	seen := make(map[string]int) // key → index in entries
	entries := make([]entry, 0, len(baseEnv))
	for _, e := range baseEnv {
		idx := strings.Index(e, "=")
		if idx < 0 {
			continue
		}
		k := e[:idx]
		v := e[idx+1:]
		if i, ok := seen[k]; ok {
			entries[i].val = v // last wins for duplicates in base
		} else {
			seen[k] = len(entries)
			entries = append(entries, entry{k, v})
		}
	}

	// Apply overrides.
	for k, v := range overrides {
		if k == "PATH" {
			// Append override PATH dirs not already present.
			existing := ""
			if i, ok := seen["PATH"]; ok {
				existing = entries[i].val
			}
			existingDirs := make(map[string]bool)
			for _, d := range strings.Split(existing, ":") {
				if d != "" {
					existingDirs[d] = true
				}
			}
			var extra []string
			for _, d := range strings.Split(v, ":") {
				if d != "" && !existingDirs[d] {
					extra = append(extra, d)
				}
			}
			if len(extra) > 0 {
				merged := existing
				if merged != "" {
					merged += ":"
				}
				merged += strings.Join(extra, ":")
				if i, ok := seen["PATH"]; ok {
					entries[i].val = merged
				} else {
					seen["PATH"] = len(entries)
					entries = append(entries, entry{"PATH", merged})
				}
			}
		} else {
			if i, ok := seen[k]; ok {
				entries[i].val = v
			} else {
				seen[k] = len(entries)
				entries = append(entries, entry{k, v})
			}
		}
	}

	result := make([]string, len(entries))
	for i, e := range entries {
		result[i] = e.key + "=" + e.val
	}
	return result
}
