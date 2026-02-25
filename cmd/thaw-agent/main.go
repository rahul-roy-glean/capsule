package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
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
	mmdsEndpoint           = flag.String("mmds-endpoint", "http://169.254.169.254", "MMDS endpoint")
	workspaceDir           = flag.String("workspace-dir", "/workspace", "Workspace directory")
	runnerDir              = flag.String("runner-dir", "/home/runner", "GitHub runner directory")
	runnerUsername         = flag.String("runner-user", "runner", "Username for GitHub runner and file ownership (e.g., 'runner' or 'gleanuser')")
	logLevel               = flag.String("log-level", "info", "Log level")
	readyFile              = flag.String("ready-file", "/var/run/thaw-agent/ready", "Ready signal file")
	skipNetwork            = flag.Bool("skip-network", false, "Skip network configuration")
	skipRunner             = flag.Bool("skip-runner", false, "Skip GitHub runner registration")
	skipRepoCache          = flag.Bool("skip-repo-cache", false, "Skip shared Bazel repository cache overlay setup")
	skipBuildbarnCerts     = flag.Bool("skip-buildbarn-certs", false, "Skip mounting Buildbarn certificate drive")
	repoCacheSeedDevice    = flag.String("repo-cache-seed-device", "/dev/vdb", "Block device for shared repo-cache seed (read-only mount inside VM)")
	repoCacheUpperDevice   = flag.String("repo-cache-upper-device", "/dev/vdc", "Block device for per-runner repo-cache upper (writable mount inside VM)")
	repoCacheSeedMount     = flag.String("repo-cache-seed-mount", "/mnt/bazel-repo-seed", "Mount point for repo-cache seed device")
	repoCacheUpperMount    = flag.String("repo-cache-upper-mount", "/mnt/bazel-repo-upper", "Mount point for repo-cache upper device")
	repoCacheOverlayTarget = flag.String("repo-cache-overlay-target", "/mnt/ephemeral/caches/repository", "Overlay mount target for Bazel --repository_cache")
	buildbarnCertsDevice   = flag.String("buildbarn-certs-device", "/dev/vdd", "Block device for Buildbarn certs drive (read-only mount inside VM)")
	buildbarnCertsMount    = flag.String("buildbarn-certs-mount", "/etc/bazel-firecracker/certs/buildbarn", "Mount point for Buildbarn certs inside the microVM")
	buildbarnCertsLabel    = flag.String("buildbarn-certs-label", "BUILDBARN_CERTS", "Filesystem label for Buildbarn certs drive")

	// Credentials flags (generic replacement for buildbarn-specific certs)
	skipCredentials   = flag.Bool("skip-credentials", false, "Skip mounting credentials drive")
	credentialsDevice = flag.String("credentials-device", "/dev/vdd", "Block device for credentials drive")
	credentialsMount  = flag.String("credentials-mount", "/mnt/credentials", "Mount point for credentials")
	credentialsLabel  = flag.String("credentials-label", "CREDENTIALS", "Filesystem label for credentials drive")

	// Git cache flags
	skipGitCache   = flag.Bool("skip-git-cache", false, "Skip git-cache setup and reference cloning")
	gitCacheDevice = flag.String("git-cache-device", "/dev/vde", "Block device for git-cache (read-only mount inside VM)")
	gitCacheMount  = flag.String("git-cache-mount", "/mnt/git-cache", "Mount point for git-cache inside the microVM")
	gitCacheLabel  = flag.String("git-cache-label", "GIT_CACHE", "Filesystem label for git-cache drive")
)

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

// RegistrationState tracks GitHub runner registration status
type RegistrationState struct {
	Attempted bool   `json:"attempted"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
	Output    string `json:"output,omitempty"`
}

var globalRegistrationState = &RegistrationState{}

// SymlinkState tracks pre-cloned repo symlink status
type SymlinkState struct {
	Attempted    bool   `json:"attempted"`
	Success      bool   `json:"success"`
	SymlinkPath  string `json:"symlink_path,omitempty"`
	TargetPath   string `json:"target_path,omitempty"`
	TargetExists bool   `json:"target_exists"`
	Error        string `json:"error,omitempty"`
}

var globalSymlinkState = &SymlinkState{}

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
		Buildbarn struct {
			CertsMountPath string `json:"certs_mount_path,omitempty"`
		} `json:"buildbarn,omitempty"`
		Network struct {
			IP        string `json:"ip"`
			Gateway   string `json:"gateway"`
			Netmask   string `json:"netmask"`
			DNS       string `json:"dns"`
			Interface string `json:"interface"`
			MAC       string `json:"mac"`
		} `json:"network"`
		Job struct {
			Repo              string            `json:"repo"`
			Branch            string            `json:"branch"`
			Commit            string            `json:"commit"`
			GitHubRunnerToken string            `json:"github_runner_token"`
			GitToken          string            `json:"git_token"`        // Installation token for git clone auth (private repos)
			GCPAccessToken    string            `json:"gcp_access_token"` // Short-lived GCP token for Artifact Registry auth
			Labels            map[string]string `json:"labels"`
		} `json:"job"`
		Snapshot struct {
			Version string `json:"version"`
		} `json:"snapshot"`
		GitCache struct {
			Enabled      bool              `json:"enabled"`
			MountPath    string            `json:"mount_path,omitempty"`
			RepoMappings map[string]string `json:"repo_mappings,omitempty"`
			WorkspaceDir string            `json:"workspace_dir,omitempty"`
			// PreClonedPath is the path where the repo was pre-cloned during warmup
			// (baked into the snapshot). Thaw-agent creates a symlink from WorkspaceDir to here.
			PreClonedPath string `json:"pre_cloned_path,omitempty"`
		} `json:"git_cache,omitempty"`
		Exec struct {
			Command    []string          `json:"command,omitempty"`
			Env        map[string]string `json:"env,omitempty"`
			WorkingDir string            `json:"working_dir,omitempty"`
			TimeoutSec int               `json:"timeout_seconds,omitempty"`
		} `json:"exec,omitempty"`
		Runner struct {
			Ephemeral bool   `json:"ephemeral"`
			CISystem  string `json:"ci_system,omitempty"`
		} `json:"runner,omitempty"`
		Warmup struct {
			Commands []snapshot.SnapshotCommand `json:"commands,omitempty"`
		} `json:"warmup,omitempty"`
		StartCommand struct {
			Command    []string `json:"command,omitempty"`
			Port       int      `json:"port,omitempty"`
			HealthPath string   `json:"health_path,omitempty"`
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
		http.HandleFunc("/service-logs", serviceLogsHandler)
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

	// Setup shared repo cache overlay (seed is shared across VMs, upper is per-VM).
	setStep("repo_cache_overlay")
	if !*skipRepoCache {
		log.Info("Setting up shared Bazel repository cache overlay...")
		if err := setupRepoCacheOverlay(); err != nil {
			log.WithError(err).Error("Failed to setup repo cache overlay")
		}
	}
	bootTimer.Phase("repo_cache_overlay")

	// Mount credentials drive (shared read-only image with certs, keys, etc.)
	if !*skipCredentials && !*skipBuildbarnCerts {
		log.Info("Mounting credentials drive...")
		if err := mountCredentials(mmdsData); err != nil {
			log.WithError(err).Error("Failed to mount credentials drive")
			// Fall back to legacy Buildbarn certs mount
			log.Info("Falling back to legacy Buildbarn certs mount...")
			if err := mountBuildbarnCerts(mmdsData); err != nil {
				log.WithError(err).Error("Failed to mount Buildbarn certs (legacy fallback)")
			}
		}
	}
	bootTimer.Phase("credentials_mount")

	// Mount git-cache for fast reference cloning
	if !*skipGitCache && mmdsData.Latest.GitCache.Enabled {
		log.Info("Mounting git-cache...")
		if err := mountGitCache(mmdsData); err != nil {
			log.WithError(err).Error("Failed to mount git-cache")
		}
	}
	bootTimer.Phase("git_cache_mount")

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

	// Mount tmpfs for workspace if needed (rootfs is often too small)
	if mmdsData.Latest.GitCache.WorkspaceDir != "" {
		workspaceDir := mmdsData.Latest.GitCache.WorkspaceDir
		if err := os.MkdirAll(workspaceDir, 0755); err == nil {
			// Check if already mounted
			if out, _ := exec.Command("mountpoint", "-q", workspaceDir).CombinedOutput(); len(out) > 0 || exec.Command("mountpoint", "-q", workspaceDir).Run() != nil {
				log.WithField("path", workspaceDir).Info("Mounting tmpfs for workspace...")
				if err := exec.Command("mount", "-t", "tmpfs", "-o", "size=3G", "tmpfs", workspaceDir).Run(); err != nil {
					log.WithError(err).Warn("Failed to mount tmpfs for workspace")
				}
			}
		}

		// Create symlink to pre-cloned repo after tmpfs mount
		// The repo is pre-cloned in the snapshot rootfs, workflow expects it at WorkspaceDir
		preClonedRepo := getPreClonedPath(mmdsData)
		if preClonedRepo != "" {
			if _, err := os.Stat(filepath.Join(preClonedRepo, ".git")); err == nil {
				symlinkPath := getWorkspaceRepoPath(mmdsData)
				if symlinkPath != "" && symlinkPath != preClonedRepo {
					if err := os.MkdirAll(filepath.Dir(symlinkPath), 0755); err == nil {
						os.RemoveAll(symlinkPath) // Remove if exists
						if err := os.Symlink(preClonedRepo, symlinkPath); err != nil {
							log.WithError(err).Warn("Failed to create symlink to pre-cloned repo")
						} else {
							log.WithFields(logrus.Fields{
								"link":   symlinkPath,
								"target": preClonedRepo,
							}).Info("Created symlink to pre-cloned repo")
						}
					}
				}
			}
		}
	}

	// Setup workspace from git-cache (local copy only, no network fetch)
	// This gives actions/checkout a head start - it only needs to fetch deltas
	setStep("git_workspace_setup")
	if mmdsData.Latest.GitCache.Enabled && mmdsData.Latest.Job.Repo != "" {
		log.Info("Setting up workspace from git-cache...")
		if err := setupWorkspaceFromGitCache(mmdsData); err != nil {
			log.WithError(err).Warn("Failed to setup workspace from git-cache, workflow will do full clone")
		}
	} else {
		log.Info("Git-cache not enabled, workflow will clone repo")
	}
	bootTimer.Phase("git_sync")

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

				// Recreate symlink after restore (tmpfs was fresh, symlink from warmup is gone)
				// Use configured paths from MMDS
				globalSymlinkState.Attempted = true

				// Use a temporary MMDSData wrapper for the helper functions
				tempData := &MMDSData{}
				tempData.Latest = newData.Latest

				preClonedRepo := getPreClonedPath(tempData)
				globalSymlinkState.TargetPath = preClonedRepo

				if preClonedRepo != "" {
					gitPath := filepath.Join(preClonedRepo, ".git")
					if _, err := os.Stat(gitPath); err == nil {
						globalSymlinkState.TargetExists = true
						symlinkPath := getWorkspaceRepoPath(tempData)
						globalSymlinkState.SymlinkPath = symlinkPath

						if symlinkPath != "" && symlinkPath != preClonedRepo {
							log.WithFields(logrus.Fields{
								"symlink": symlinkPath,
								"target":  preClonedRepo,
							}).Info("Creating symlink to pre-cloned repo after restore")

							if err := os.MkdirAll(filepath.Dir(symlinkPath), 0755); err != nil {
								globalSymlinkState.Error = fmt.Sprintf("MkdirAll failed: %v", err)
								log.WithError(err).Error("Failed to create symlink parent dir")
							} else {
								os.RemoveAll(symlinkPath) // Remove if exists
								if err := os.Symlink(preClonedRepo, symlinkPath); err != nil {
									globalSymlinkState.Error = fmt.Sprintf("Symlink failed: %v", err)
									log.WithError(err).Error("Failed to create post-restore symlink")
								} else {
									globalSymlinkState.Success = true
									log.Info("Successfully created symlink to pre-cloned repo")
								}
							}
						}
					} else {
						globalSymlinkState.Error = fmt.Sprintf("Target .git not found: %v", err)
						log.WithFields(logrus.Fields{
							"path":  gitPath,
							"error": err,
						}).Warn("Pre-cloned repo .git not found for symlink")
					}
				} else {
					log.Debug("No pre-cloned path configured, skipping symlink")
				}

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

				// Sync clock from MMDS current_time BEFORE runner registration.
				// The host sets current_time when building MMDS data. Without this,
				// config.sh fails with "clock may be out of sync" because the guest
				// clock is stuck at snapshot creation time.
				if ct := newData.Latest.Meta.CurrentTime; ct != "" {
					if hostTime, err := time.Parse(time.RFC3339, ct); err == nil {
						tv := syscall.Timeval{Sec: hostTime.Unix()}
						if err := syscall.Settimeofday(&tv); err == nil {
							log.WithField("server_time", hostTime.UTC().Format(time.RFC3339)).Info("Clock synced from MMDS after snapshot restore (pre-registration)")
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

	// Run CI runner registration
	setStep("ci_registration")
	ciSystem := mmdsData.Latest.Runner.CISystem
	if ciSystem == "" && mmdsData.Latest.Job.GitHubRunnerToken != "" {
		ciSystem = "github-actions" // backwards compat
	}

	if !*skipRunner {
		switch ciSystem {
		case "github-actions":
			if mmdsData.Latest.Job.GitHubRunnerToken != "" {
				log.Info("Registering GitHub Actions runner...")
				globalRegistrationState.Attempted = true
				// Retry registration up to 3 times with backoff.
				// Transient GitHub API errors ("Resource temporarily unavailable")
				// are common when multiple VMs register simultaneously.
				var regErr error
				for attempt := 1; attempt <= 3; attempt++ {
					if attempt > 1 {
						delay := time.Duration(attempt*5) * time.Second
						log.WithFields(logrus.Fields{
							"attempt": attempt,
							"delay":   delay,
						}).Info("Retrying runner registration...")
						time.Sleep(delay)
					}
					regErr = registerGitHubRunner(mmdsData)
					if regErr == nil {
						break
					}
					log.WithError(regErr).WithField("attempt", attempt).Warn("Runner registration attempt failed")
				}
				if regErr != nil {
					globalRegistrationState.Error = regErr.Error()
					log.WithError(regErr).Error("Failed to register GitHub runner after all retries")
				} else {
					globalRegistrationState.Success = true
				}
			} else {
				log.Info("GitHub Actions CI system configured but no runner token provided, skipping registration")
			}
		case "none", "":
			log.Info("No CI system configured, skipping runner registration")
		default:
			log.WithField("ci_system", ciSystem).Warn("Unknown CI system, skipping runner registration")
		}
	}

	bootTimer.Phase("ci_runner")

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

func setupRepoCacheOverlay() error {
	// Ensure mount points exist
	if err := os.MkdirAll(*repoCacheSeedMount, 0755); err != nil {
		return fmt.Errorf("failed to create seed mount dir: %w", err)
	}
	if err := os.MkdirAll(*repoCacheUpperMount, 0755); err != nil {
		return fmt.Errorf("failed to create upper mount dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(*repoCacheOverlayTarget), 0755); err != nil {
		return fmt.Errorf("failed to create overlay target parent dir: %w", err)
	}
	if err := os.MkdirAll(*repoCacheOverlayTarget, 0755); err != nil {
		return fmt.Errorf("failed to create overlay target dir: %w", err)
	}

	seedDev := resolveDevice(*repoCacheSeedDevice, "BAZEL_REPO_SEED")
	upperDev := resolveDevice(*repoCacheUpperDevice, "BAZEL_REPO_UPPER")

	// Mount seed read-only (safe to share)
	// Ignore if already mounted.
	exec.Command("mountpoint", "-q", *repoCacheSeedMount).Run()
	if err := exec.Command("mount", "-o", "ro", seedDev, *repoCacheSeedMount).Run(); err != nil {
		// If mount fails because it's already mounted, proceed.
		log.WithError(err).WithFields(logrus.Fields{
			"device": seedDev,
			"mount":  *repoCacheSeedMount,
		}).Warn("Seed mount failed (may already be mounted)")
	}

	// Mount upper read-write
	exec.Command("mountpoint", "-q", *repoCacheUpperMount).Run()
	if err := exec.Command("mount", upperDev, *repoCacheUpperMount).Run(); err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			"device": upperDev,
			"mount":  *repoCacheUpperMount,
		}).Warn("Upper mount failed (may already be mounted)")
	}

	upperDir := filepath.Join(*repoCacheUpperMount, "upper")
	workDir := filepath.Join(*repoCacheUpperMount, "work")
	if err := os.MkdirAll(upperDir, 0755); err != nil {
		return fmt.Errorf("failed to create overlay upper dir: %w", err)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("failed to create overlay work dir: %w", err)
	}

	// Mount overlayfs at Bazel repository_cache path
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", *repoCacheSeedMount, upperDir, workDir)
	if output, err := exec.Command("mount", "-t", "overlay", "overlay", "-o", opts, *repoCacheOverlayTarget).CombinedOutput(); err != nil {
		return fmt.Errorf("overlay mount failed: %s: %w", string(output), err)
	}

	// Ensure the runner user can write into the repo cache path without recursively
	// chowning (which would copy-up most of the seed into the upper layer).
	_ = exec.Command("chown", *runnerUsername+":"+*runnerUsername, *repoCacheOverlayTarget).Run()
	// Also chown the upper mount so bazel can create disk-cache dir under it.
	_ = exec.Command("chown", *runnerUsername+":"+*runnerUsername, *repoCacheUpperMount).Run()

	log.WithFields(logrus.Fields{
		"seed_device":  seedDev,
		"seed_mount":   *repoCacheSeedMount,
		"upper_device": upperDev,
		"upper_mount":  *repoCacheUpperMount,
		"target":       *repoCacheOverlayTarget,
	}).Info("Repo cache overlay mounted")

	return nil
}

func mountBuildbarnCerts(data *MMDSData) error {
	mountPath := *buildbarnCertsMount
	if data != nil && data.Latest.Buildbarn.CertsMountPath != "" {
		mountPath = data.Latest.Buildbarn.CertsMountPath
	}
	if mountPath == "" {
		return nil
	}

	if err := os.MkdirAll(mountPath, 0755); err != nil {
		return fmt.Errorf("failed to create buildbarn certs mount dir: %w", err)
	}

	dev := resolveDevice(*buildbarnCertsDevice, *buildbarnCertsLabel)
	if err := exec.Command("mountpoint", "-q", mountPath).Run(); err == nil {
		return nil
	}
	if output, err := exec.Command("mount", "-o", "ro", dev, mountPath).CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %s: %w", string(output), err)
	}

	log.WithFields(logrus.Fields{
		"device": dev,
		"mount":  mountPath,
	}).Info("Buildbarn certs mounted")
	return nil
}

// mountCredentials mounts the generic credentials drive and sets up symlinks.
func mountCredentials(data *MMDSData) error {
	mountPath := *credentialsMount

	if err := os.MkdirAll(mountPath, 0755); err != nil {
		return fmt.Errorf("failed to create credentials mount dir: %w", err)
	}

	// Try new CREDENTIALS label first, fall back to legacy BUILDBARN_CERTS
	dev := resolveDevice(*credentialsDevice, *credentialsLabel)
	if _, err := os.Stat(dev); err != nil {
		dev = resolveDevice(*buildbarnCertsDevice, *buildbarnCertsLabel)
	}

	if err := exec.Command("mountpoint", "-q", mountPath).Run(); err == nil {
		return nil // already mounted
	}
	if output, err := exec.Command("mount", "-o", "ro", dev, mountPath).CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %s: %w", string(output), err)
	}

	log.WithFields(logrus.Fields{
		"device": dev,
		"mount":  mountPath,
	}).Info("Credentials drive mounted")

	// Setup credential symlinks and environment
	setupCredentialSymlinks(mountPath, data)

	return nil
}

// setupCredentialSymlinks creates symlinks and environment setup from the credentials drive.
func setupCredentialSymlinks(mountPath string, data *MMDSData) {
	runnerHome := "/home/" + *runnerUsername

	// Symlink .netrc if present
	netrcPath := filepath.Join(mountPath, "netrc")
	if _, err := os.Stat(netrcPath); err == nil {
		target := filepath.Join(runnerHome, ".netrc")
		os.Remove(target)
		if err := os.Symlink(netrcPath, target); err != nil {
			log.WithError(err).Warn("Failed to symlink .netrc")
		} else {
			log.Info("Linked .netrc from credentials drive")
		}
	}

	// Symlink git-credentials if present
	gitCredsPath := filepath.Join(mountPath, "git-credentials")
	if _, err := os.Stat(gitCredsPath); err == nil {
		target := filepath.Join(runnerHome, ".git-credentials")
		os.Remove(target)
		if err := os.Symlink(gitCredsPath, target); err != nil {
			log.WithError(err).Warn("Failed to symlink .git-credentials")
		} else {
			log.Info("Linked .git-credentials from credentials drive")
		}
		// Configure git to use credential store
		exec.Command("git", "config", "--global", "credential.helper", "store").Run()
	}

	// Source environment file if present
	envPath := filepath.Join(mountPath, "env")
	if envData, err := os.ReadFile(envPath); err == nil {
		for k, v := range parseEnvFile(envData) {
			os.Setenv(k, v)
			log.WithField("var", k).Debug("Set environment variable from credentials")
		}
	}

	// Install CA certs if present
	caBundlePath := filepath.Join(mountPath, "certs", "ca-bundle")
	if _, err := os.Stat(caBundlePath); err == nil {
		entries, _ := os.ReadDir(caBundlePath)
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".crt") {
				src := filepath.Join(caBundlePath, entry.Name())
				dst := filepath.Join("/usr/local/share/ca-certificates", entry.Name())
				exec.Command("cp", src, dst).Run()
			}
		}
		if len(entries) > 0 {
			exec.Command("update-ca-certificates").Run()
			log.Info("Installed CA certificates from credentials drive")
		}
	}

	// Copy .npmrc if present
	npmrcPath := filepath.Join(mountPath, "npm", ".npmrc")
	if _, err := os.Stat(npmrcPath); err == nil {
		target := filepath.Join(runnerHome, ".npmrc")
		exec.Command("cp", npmrcPath, target).Run()
		exec.Command("chown", *runnerUsername+":"+*runnerUsername, target).Run()
		log.Info("Copied .npmrc from credentials drive")
	}

	// Backwards compatibility: if buildbarn certs exist in credentials drive,
	// create legacy mount path symlink
	buildbarnPath := filepath.Join(mountPath, "certs", "buildbarn")
	if _, err := os.Stat(buildbarnPath); err == nil {
		legacyMount := data.Latest.Buildbarn.CertsMountPath
		if legacyMount == "" {
			legacyMount = *buildbarnCertsMount
		}
		if legacyMount != "" && legacyMount != buildbarnPath {
			os.MkdirAll(filepath.Dir(legacyMount), 0755)
			os.Remove(legacyMount)
			if err := os.Symlink(buildbarnPath, legacyMount); err != nil {
				log.WithError(err).Warn("Failed to create legacy buildbarn certs symlink")
			} else {
				log.WithFields(logrus.Fields{
					"link":   legacyMount,
					"target": buildbarnPath,
				}).Info("Created legacy Buildbarn certs symlink")
			}
		}
	}
}

func resolveDevice(defaultDev string, label string) string {
	// Prefer by-label path if present.
	byLabel := filepath.Join("/dev/disk/by-label", label)
	if _, err := os.Stat(byLabel); err == nil {
		return byLabel
	}
	// Fall back to default device path.
	return defaultDev
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
				Buildbarn struct {
					CertsMountPath string `json:"certs_mount_path,omitempty"`
				} `json:"buildbarn,omitempty"`
				Network struct {
					IP        string `json:"ip"`
					Gateway   string `json:"gateway"`
					Netmask   string `json:"netmask"`
					DNS       string `json:"dns"`
					Interface string `json:"interface"`
					MAC       string `json:"mac"`
				} `json:"network"`
				Job struct {
					Repo              string            `json:"repo"`
					Branch            string            `json:"branch"`
					Commit            string            `json:"commit"`
					GitHubRunnerToken string            `json:"github_runner_token"`
					GitToken          string            `json:"git_token"`
					GCPAccessToken    string            `json:"gcp_access_token"`
					Labels            map[string]string `json:"labels"`
				} `json:"job"`
				Snapshot struct {
					Version string `json:"version"`
				} `json:"snapshot"`
				GitCache struct {
					Enabled       bool              `json:"enabled"`
					MountPath     string            `json:"mount_path,omitempty"`
					RepoMappings  map[string]string `json:"repo_mappings,omitempty"`
					WorkspaceDir  string            `json:"workspace_dir,omitempty"`
					PreClonedPath string            `json:"pre_cloned_path,omitempty"`
				} `json:"git_cache,omitempty"`
				Exec struct {
					Command    []string          `json:"command,omitempty"`
					Env        map[string]string `json:"env,omitempty"`
					WorkingDir string            `json:"working_dir,omitempty"`
					TimeoutSec int               `json:"timeout_seconds,omitempty"`
				} `json:"exec,omitempty"`
				Runner struct {
					Ephemeral bool   `json:"ephemeral"`
					CISystem  string `json:"ci_system,omitempty"`
				} `json:"runner,omitempty"`
				Warmup struct {
					Commands []snapshot.SnapshotCommand `json:"commands,omitempty"`
				} `json:"warmup,omitempty"`
				StartCommand struct {
					Command    []string `json:"command,omitempty"`
					Port       int      `json:"port,omitempty"`
					HealthPath string   `json:"health_path,omitempty"`
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
		"runner_id":     data.Latest.Meta.RunnerID,
		"mode":          data.Latest.Meta.Mode,
		"job_repo":      data.Latest.Job.Repo,
		"has_git_token": data.Latest.Job.GitToken != "",
		"git_token_len": len(data.Latest.Job.GitToken),
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

	// Check if kernel already configured the network (via ip= boot parameter)
	// If so, skip IP reconfiguration but still ensure DNS is configured
	out, _ := exec.Command("ip", "addr", "show", "dev", iface).Output()
	expectedIP := strings.Split(net.IP, "/")[0]
	if strings.Contains(string(out), expectedIP) {
		log.WithField("ip", expectedIP).Info("Network IP already configured by kernel, ensuring DNS is set")
		// Still configure DNS since kernel ip= parameter doesn't set it
		if net.DNS != "" {
			resolv := fmt.Sprintf("nameserver %s\n", net.DNS)
			if err := os.WriteFile("/etc/resolv.conf", []byte(resolv), 0644); err != nil {
				log.WithError(err).Warn("Failed to write resolv.conf")
			}
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
// guest clock when it appears. After snapshot restore, the thaw-agent process
// resumes from where it was paused (at select{} in main). The host manager
// sets new MMDS data (including current_time) after restore. This goroutine
// detects that and syncs the clock so GitHub runner registration can succeed.
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

func mountGitCache(data *MMDSData) error {
	mountPath := *gitCacheMount
	if data != nil && data.Latest.GitCache.MountPath != "" {
		mountPath = data.Latest.GitCache.MountPath
	}
	if mountPath == "" {
		return nil
	}

	if err := os.MkdirAll(mountPath, 0755); err != nil {
		return fmt.Errorf("failed to create git-cache mount dir: %w", err)
	}

	dev := resolveDevice(*gitCacheDevice, *gitCacheLabel)

	// Check if already mounted
	if err := exec.Command("mountpoint", "-q", mountPath).Run(); err == nil {
		log.WithField("mount", mountPath).Debug("Git-cache already mounted")
		return nil
	}

	if output, err := exec.Command("mount", "-o", "ro", dev, mountPath).CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %s: %w", string(output), err)
	}

	log.WithFields(logrus.Fields{
		"device": dev,
		"mount":  mountPath,
	}).Info("Git-cache mounted")
	return nil
}

// setupWorkspaceFromGitCache copies the git-cache to workspace (local only, no network)
// This gives actions/checkout a huge head start - it only needs to fetch deltas
func setupWorkspaceFromGitCache(data *MMDSData) error {
	job := data.Latest.Job
	if job.Repo == "" {
		return nil
	}

	// Determine paths
	gitCachePath := *gitCacheMount
	if data.Latest.GitCache.MountPath != "" {
		gitCachePath = data.Latest.GitCache.MountPath
	}

	workspacePath := *workspaceDir
	if data.Latest.GitCache.WorkspaceDir != "" {
		workspacePath = data.Latest.GitCache.WorkspaceDir
	}

	// Find the cached repo
	// Git-cache uses simple repo name: /mnt/git-cache/scio (from git_cache_repos config)
	// Workspace uses GitHub Actions convention: /mnt/ephemeral/workdir/scio/scio
	repoFullPath := extractRepoDir(job.Repo) // Returns "scio/scio" for askscio/scio
	parts := strings.Split(job.Repo, "/")
	simpleRepoName := parts[len(parts)-1] // Just "scio"

	cachePath := filepath.Join(gitCachePath, simpleRepoName) // /mnt/git-cache/scio
	targetPath := filepath.Join(workspacePath, repoFullPath) // /mnt/ephemeral/workdir/scio/scio

	// Check if git-cache has this repo
	if _, err := os.Stat(filepath.Join(cachePath, ".git")); os.IsNotExist(err) {
		return fmt.Errorf("repo not found in git-cache: %s", cachePath)
	}

	log.WithFields(logrus.Fields{
		"cache":  cachePath,
		"target": targetPath,
		"repo":   job.Repo,
	}).Info("Copying git-cache to workspace")

	// Create workspace directory
	if err := os.MkdirAll(workspacePath, 0755); err != nil {
		return fmt.Errorf("failed to create workspace: %w", err)
	}

	// Use git clone --reference for efficient local copy
	// --dissociate makes it independent (copies objects instead of linking)
	// --no-checkout is fast, actions/checkout will do the checkout
	cloneCmd := exec.Command("git", "clone",
		"--reference", cachePath,
		"--dissociate",
		"--no-checkout",
		"file://"+cachePath, // Local clone
		targetPath,
	)
	cloneCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	if output, err := cloneCmd.CombinedOutput(); err != nil {
		// If target exists, try to set it up as alternates instead
		if _, statErr := os.Stat(targetPath); statErr == nil {
			log.Info("Target exists, setting up alternates instead")
			return setupGitAlternates(targetPath, cachePath)
		}
		return fmt.Errorf("git clone failed: %s: %w", string(output), err)
	}

	// Set remote to the real GitHub URL (so fetch works)
	repoURL := "https://github.com/" + job.Repo
	if err := exec.Command("git", "-C", targetPath, "remote", "set-url", "origin", repoURL).Run(); err != nil {
		log.WithError(err).Warn("Failed to set remote URL")
	}

	// Make it writable for the runner user
	exec.Command("chown", "-R", *runnerUsername+":"+*runnerUsername, targetPath).Run()

	log.WithField("target", targetPath).Info("Workspace setup from git-cache complete")
	return nil
}

// setupGitAlternates configures an existing repo to use git-cache objects
func setupGitAlternates(repoPath, cachePath string) error {
	alternatesFile := filepath.Join(repoPath, ".git", "objects", "info", "alternates")
	cacheObjects := filepath.Join(cachePath, ".git", "objects")

	if err := os.MkdirAll(filepath.Dir(alternatesFile), 0755); err != nil {
		return err
	}

	return os.WriteFile(alternatesFile, []byte(cacheObjects+"\n"), 0644)
}

func findGitCacheReference(data *MMDSData, repoURL string) string {
	gitCache := data.Latest.GitCache
	if !gitCache.Enabled {
		return ""
	}

	mountPath := gitCache.MountPath
	if mountPath == "" {
		mountPath = *gitCacheMount
	}

	// Check repo mappings first
	for pattern, cacheName := range gitCache.RepoMappings {
		if strings.Contains(repoURL, pattern) || pattern == repoURL {
			refPath := filepath.Join(mountPath, cacheName)
			if _, err := os.Stat(filepath.Join(refPath, ".git")); err == nil {
				return refPath
			}
			// Also try bare repo
			if _, err := os.Stat(filepath.Join(refPath, "HEAD")); err == nil {
				return refPath
			}
		}
	}

	// Try to infer from repo URL - extractRepoDir returns repo/repo, we need just repo
	repoPath := extractRepoDir(repoURL) // scio/scio
	repoName := filepath.Base(repoPath) // scio
	candidates := []string{
		filepath.Join(mountPath, repoName),        // /mnt/git-cache/scio
		filepath.Join(mountPath, repoName+".git"), // /mnt/git-cache/scio.git
	}

	for _, candidate := range candidates {
		// Check for regular clone
		if _, err := os.Stat(filepath.Join(candidate, ".git")); err == nil {
			return candidate
		}
		// Check for bare repo
		if _, err := os.Stat(filepath.Join(candidate, "HEAD")); err == nil {
			return candidate
		}
	}

	return ""
}

func extractRepoDir(repoURL string) string {
	// Handle various URL formats - returns repo/repo structure for GitHub Actions compatibility
	// GitHub Actions default checkout is: $GITHUB_WORKSPACE/{repo}/{repo}
	// https://github.com/org/repo.git -> repo/repo
	// askscio/scio -> scio/scio

	repoURL = strings.TrimSuffix(repoURL, ".git")
	repoURL = strings.TrimPrefix(repoURL, "https://")
	repoURL = strings.TrimPrefix(repoURL, "http://")
	repoURL = strings.TrimPrefix(repoURL, "git@")
	repoURL = strings.TrimPrefix(repoURL, "github.com/")
	repoURL = strings.TrimPrefix(repoURL, "github.com:")

	// Extract just the repo name (last part)
	parts := strings.Split(repoURL, "/")
	repoName := parts[len(parts)-1]

	// Return repo/repo format (GitHub Actions convention)
	return filepath.Join(repoName, repoName)
}

func registerGitHubRunner(data *MMDSData) error {
	job := data.Latest.Job
	if job.GitHubRunnerToken == "" {
		return fmt.Errorf("no GitHub runner token")
	}

	runnerPath := *runnerDir

	// Extract repo URL for registration
	repoURL := job.Repo
	if !strings.HasPrefix(repoURL, "https://") {
		repoURL = "https://github.com/" + repoURL
	}

	// Build labels - GitHub expects just label names, not key=value pairs
	var labels []string
	for k := range job.Labels {
		labels = append(labels, k)
	}
	// Add host machine name as a label for easier debugging
	if hostName := data.Latest.Meta.InstanceName; hostName != "" {
		labels = append(labels, hostName)
	}
	labelsStr := strings.Join(labels, ",")

	// Get runner user UID/GID - GitHub runner refuses to run as root
	runnerUser, err := user.Lookup(*runnerUsername)
	if err != nil {
		return fmt.Errorf("runner user not found: %w", err)
	}
	uid, _ := strconv.ParseUint(runnerUser.Uid, 10, 32)
	gid, _ := strconv.ParseUint(runnerUser.Gid, 10, 32)

	// NOTE: Runner directory ownership is set in the Dockerfile (chown -R gleanuser:gleanuser /home/gleanuser)
	// and preserved across snapshot restore. No recursive chown needed here.

	// Build config command arguments
	configArgs := []string{
		"--url", repoURL,
		"--token", job.GitHubRunnerToken,
		"--name", data.Latest.Meta.RunnerID[:8],
		"--labels", labelsStr,
		"--unattended",
		"--replace",
	}
	// Add --ephemeral flag if configured (defaults to true if not set)
	if data.Latest.Runner.Ephemeral {
		configArgs = append(configArgs, "--ephemeral")
		log.Info("Runner configured as ephemeral (one job per VM)")
	} else {
		log.Info("Runner configured as persistent (multiple jobs)")
	}

	// Configure runner as 'runner' user with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	configCmd := exec.CommandContext(ctx, filepath.Join(runnerPath, "config.sh"), configArgs...)
	configCmd.Dir = runnerPath
	configCmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
	}
	configCmd.Env = append(os.Environ(), "HOME="+runnerUser.HomeDir)

	log.WithFields(logrus.Fields{
		"url":      repoURL,
		"name":     data.Latest.Meta.RunnerID[:8],
		"labels":   labelsStr,
		"uid":      uid,
		"gid":      gid,
		"home":     runnerUser.HomeDir,
		"run_path": runnerPath,
	}).Info("Configuring GitHub runner (timeout: 120s)...")

	configStart := time.Now()
	output, err := configCmd.CombinedOutput()
	if err != nil {
		log.WithFields(logrus.Fields{
			"error":       err.Error(),
			"output":      string(output),
			"duration_ms": time.Since(configStart).Milliseconds(),
		}).Error("config.sh failed")
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("runner config timed out after 120s")
		}
		return fmt.Errorf("runner config failed: %w (output: %s)", err, string(output))
	}
	log.WithFields(logrus.Fields{
		"output":      string(output),
		"duration_ms": time.Since(configStart).Milliseconds(),
	}).Info("config.sh completed successfully")

	// Start runner in background as 'runner' user
	// Use setsid to create a new session so runner survives if thaw-agent exits
	runCmd := exec.Command(filepath.Join(runnerPath, "run.sh"), "--disableupdate")
	runCmd.Dir = runnerPath
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stderr
	runCmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
		Setsid: true, // Create new session so runner survives parent exit
	}
	runCmd.Env = append(os.Environ(), "HOME="+runnerUser.HomeDir)

	log.Info("Starting GitHub runner (run.sh)...")
	runStart := time.Now()
	if err := runCmd.Start(); err != nil {
		return fmt.Errorf("failed to start runner: %w", err)
	}
	log.WithFields(logrus.Fields{
		"pid":         runCmd.Process.Pid,
		"duration_ms": time.Since(runStart).Milliseconds(),
	}).Info("GitHub runner started successfully")

	return nil
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

	// Infra finalization: GitHub Actions runner update and page pre-warm.
	// Only relevant for github-actions CI system (or when a runner token is present).
	ciSystem := data.Latest.Runner.CISystem
	if ciSystem == "" && data.Latest.Job.GitHubRunnerToken != "" {
		ciSystem = "github-actions"
	}
	if ciSystem == "github-actions" {
		runnerUser, err := user.Lookup(*runnerUsername)
		if err != nil {
			return fmt.Errorf("runner user not found: %w", err)
		}
		rUID, _ := strconv.ParseUint(runnerUser.Uid, 10, 32)
		rGID, _ := strconv.ParseUint(runnerUser.Gid, 10, 32)
		runnerCred := &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(rUID), Gid: uint32(rGID)},
		}
		runnerHome := "HOME=" + runnerUser.HomeDir

		updateWarmupState("runner_update", "Updating GitHub Actions runner...")
		updateGitHubRunner(*runnerDir, runnerCred, runnerHome)

		updateWarmupState("runner_prewarm", "Pre-warming runner pages...")
		preWarmRunnerPages(*runnerDir, runnerCred, runnerHome)
	}

	updateWarmupState("syncing", "Syncing caches to disk...")
	exec.Command("sync").Run()

	return nil
}

// dispatchCommand routes a SnapshotCommand to its handler.
func dispatchCommand(cmd snapshot.SnapshotCommand, data *MMDSData) error {
	switch cmd.Type {
	case "git-clone":
		return runGitCloneCommand(cmd.Args, data)
	case "gcp-auth":
		return runGCPAuthCommand(data)
	case "shell":
		return runShellCommand(cmd.Args, cmd.RunAsRoot, data)
	default:
		return fmt.Errorf("unknown command type: %q", cmd.Type)
	}
}

// runShellCommand runs an arbitrary shell command with MMDS credentials
// injected as environment variables so callers can use $GOOGLE_OAUTH_ACCESS_TOKEN,
// $GIT_TOKEN, etc. without any extra setup steps.
// When runAsRoot is false the command is run as the configured runner user.
func runShellCommand(args []string, runAsRoot bool, data *MMDSData) error {
	if len(args) == 0 {
		return fmt.Errorf("shell command requires at least one argument")
	}
	env := os.Environ()
	if data != nil {
		if t := data.Latest.Job.GCPAccessToken; t != "" {
			env = append(env, "GOOGLE_OAUTH_ACCESS_TOKEN="+t)
			env = append(env, "CLOUDSDK_AUTH_ACCESS_TOKEN="+t)
		}
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

// runGCPAuthCommand configures GCP credentials inside the microVM by:
//   - Setting environment variables (GOOGLE_OAUTH_ACCESS_TOKEN, CLOUDSDK_AUTH_ACCESS_TOKEN)
//   - Installing a gcloud shim at /usr/local/bin/gcloud that returns the access token
//
// The gcloud shim is critical because keyrings.google-artifactregistry-auth (used by
// pip inside bazel) calls `gcloud config config-helper --format=json(credential)` to
// obtain credentials. Since gcloud isn't installed in the microVM and there's no
// metadata server, the shim fakes the expected responses.
func runGCPAuthCommand(data *MMDSData) error {
	token := data.Latest.Job.GCPAccessToken
	if token == "" {
		return fmt.Errorf("gcp-auth: no GCP access token in MMDS (gcp_access_token is empty)")
	}

	// Set env vars for tools that check them directly.
	os.Setenv("GOOGLE_OAUTH_ACCESS_TOKEN", token)
	os.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", token)

	// Install a gcloud shim that returns the access token for all relevant subcommands.
	// This makes keyrings.google-artifactregistry-auth and bazel credential helpers work
	// without a real gcloud installation.
	shimScript := fmt.Sprintf(`#!/bin/sh
# Shim installed by thaw-agent for Artifact Registry auth in microVM
# Returns the access token passed via MMDS from the host
case "$1" in
  auth)
    case "$2" in
      print-access-token) echo '%s' ;;
      application-default) echo '%s' ;;
      *) echo '%s' ;;
    esac
    ;;
  config)
    # keyrings.google-artifactregistry-auth calls:
    #   gcloud config config-helper --format=json(credential)
    # and parses credential.access_token + credential.token_expiry from JSON
    echo '{"credential":{"access_token":"%s","token_expiry":"2099-12-31T23:59:59Z"}}'
    ;;
  *) echo '%s' ;;
esac
`, token, token, token, token, token)

	shimPath := filepath.Join("/usr/local/bin", "gcloud")
	if err := os.WriteFile(shimPath, []byte(shimScript), 0755); err != nil {
		log.WithError(err).Warn("Failed to write gcloud shim")
	} else {
		log.Info("Installed gcloud shim for credential helper")
	}

	log.Info("GCP credentials configured (env vars + gcloud shim)")
	return nil
}

// runGitCloneCommand implements the "git-clone" warmup step.
// args[0] = repo URL, args[1] = branch (optional), args[2] = warmup targets (optional),
// args[3] = bazelrc path relative to repo root (optional)
func runGitCloneCommand(args []string, data *MMDSData) error {
	if len(args) == 0 {
		return fmt.Errorf("git-clone command requires a repo URL argument")
	}
	repoURL := args[0]
	repoBranch := "main"
	if len(args) > 1 && args[1] != "" {
		repoBranch = args[1]
	}
	// Store bazelrc and warmup targets for use by the caller
	// (they'll be read from args by runBazelFetchCommand)

	// Look up runner user
	runnerUser, err := user.Lookup(*runnerUsername)
	if err != nil {
		return fmt.Errorf("runner user not found: %w", err)
	}
	rUID, _ := strconv.ParseUint(runnerUser.Uid, 10, 32)
	rGID, _ := strconv.ParseUint(runnerUser.Gid, 10, 32)
	runnerCred := &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(rUID), Gid: uint32(rGID)},
	}
	runnerHome := "HOME=" + runnerUser.HomeDir

	repoPath := extractRepoDir(repoURL)
	actualRepoDir := filepath.Join("/workspace", repoPath)
	workDir := data.Latest.GitCache.WorkspaceDir
	if workDir == "" {
		workDir = "/mnt/ephemeral/workdir"
	}
	expectedRepoDir := filepath.Join(workDir, repoPath)
	repoDir := actualRepoDir

	log.WithFields(logrus.Fields{
		"actual_repo_dir":   actualRepoDir,
		"expected_repo_dir": expectedRepoDir,
	}).Info("Setting up repo directories for warmup")

	updateWarmupState("connectivity_check", "Verifying network connectivity...")
	if err := verifyConnectivity(repoURL); err != nil {
		return fmt.Errorf("connectivity check failed: %w", err)
	}

	updateWarmupState("cloning", "Cloning repository...")
	log.WithFields(logrus.Fields{
		"repo_url": repoURL,
		"branch":   repoBranch,
		"repo_dir": repoDir,
	}).Info("Cloning repository for warmup")

	parentDir := filepath.Dir(repoDir)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("failed to create repo parent dir: %w", err)
	}
	os.Chown(parentDir, int(rUID), int(rGID))

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		log.Info("Repository already exists, skipping clone")
	} else {
		var cloned bool
		if data.Latest.GitCache.Enabled {
			cachePath := findGitCacheReference(data, repoURL)
			if cachePath != "" {
				cloneCmd := exec.Command("git", "clone", "--branch", repoBranch, "file://"+cachePath, repoDir)
				cloneCmd.SysProcAttr = runnerCred
				cloneCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", runnerHome)
				output, err := cloneCmd.CombinedOutput()
				if err != nil {
					log.WithFields(logrus.Fields{"error": err.Error(), "output": string(output)}).Warn("Local clone from git-cache failed, will try network clone")
				} else {
					setURLCmd := exec.Command("git", "-C", repoDir, "remote", "set-url", "origin", "https://github.com/"+repoURL)
					setURLCmd.SysProcAttr = runnerCred
					setURLCmd.Env = append(os.Environ(), runnerHome)
					setURLCmd.Run()
					cloned = true
				}
			}
		}
		if !cloned {
			cloneURL := repoURL
			if data.Latest.Job.GitToken != "" {
				repoPath := strings.TrimPrefix(repoURL, "https://github.com/")
				repoPath = strings.TrimPrefix(repoPath, "github.com/")
				cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s", data.Latest.Job.GitToken, repoPath)
			} else if !strings.HasPrefix(cloneURL, "https://") && !strings.HasPrefix(cloneURL, "git@") {
				cloneURL = "https://github.com/" + cloneURL
			}
			if err := runStreamedCommand("", append(os.Environ(), runnerHome, "GIT_TERMINAL_PROMPT=0"), runnerCred,
				"git", "clone", "--branch", repoBranch, "--depth=1", cloneURL, repoDir); err != nil {
				return fmt.Errorf("git clone failed: %w", err)
			}
		}
	}

	if actualRepoDir != expectedRepoDir {
		if err := os.MkdirAll(filepath.Dir(expectedRepoDir), 0755); err == nil {
			os.RemoveAll(expectedRepoDir)
			os.Symlink(actualRepoDir, expectedRepoDir)
		}
	}
	return nil
}

func preWarmRunnerPages(runnerPath string, cred *syscall.SysProcAttr, homeEnv string) {
	start := time.Now()
	log.WithField("runner_path", runnerPath).Info("Pre-warming .NET runner pages...")
	globalWarmupLogs.Add("[phase] runner page pre-warm")

	listenerBin := filepath.Join(runnerPath, "bin", "Runner.Listener")
	if _, err := os.Stat(listenerBin); err != nil {
		log.WithError(err).Warn("Runner.Listener binary not found, skipping pre-warm")
		globalWarmupLogs.Add("[warn] Runner.Listener not found: " + err.Error())
		return
	}

	// Start Runner.Listener briefly. It will fail quickly because config.sh
	// hasn't been run (no .runner config file), but that's fine — the .NET
	// runtime, JIT compiler, and managed assemblies all get loaded into memory
	// before it exits, which is exactly what we need for the snapshot.
	// Give it up to 15 seconds — .NET runtime typically loads within a few seconds,
	// then Runner.Listener exits with an error because it's not configured.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, listenerBin)
	cmd.Dir = runnerPath
	cmd.Env = append(os.Environ(), homeEnv, "DOTNET_EnableDiagnostics=0")
	if cred != nil {
		cmd.SysProcAttr = cred
	}

	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	// We expect it to fail (no config) — that's fine, pages are loaded.
	log.WithFields(logrus.Fields{
		"duration_ms": elapsed.Milliseconds(),
		"exit_error":  err,
		"output":      strings.TrimSpace(string(out)),
	}).Info("Runner.Listener pre-warm completed")
	globalWarmupLogs.Add(fmt.Sprintf("[done] runner pre-warm completed in %dms", elapsed.Milliseconds()))
}

// updateGitHubRunner downloads the latest GitHub Actions runner release and
// replaces the existing installation. This avoids a ~3-4 minute self-update
// delay after snapshot restore. The runner is downloaded directly from GitHub
// releases API rather than relying on run.sh's self-update mechanism (which
// requires the runner to be configured and connected first).
func updateGitHubRunner(runnerPath string, cred *syscall.SysProcAttr, homeEnv string) {
	start := time.Now()
	log.WithField("runner_path", runnerPath).Info("Checking for GitHub Actions runner updates...")
	globalWarmupLogs.Add("[phase] runner update check")

	binDir := filepath.Join(runnerPath, "bin")
	if _, err := os.Stat(binDir); err != nil {
		log.WithError(err).Warn("Runner bin dir not found, skipping update")
		globalWarmupLogs.Add("[warn] runner bin dir not found, skipping update")
		return
	}

	// Read current version from Runner.Listener.deps.json filename pattern or
	// by running Runner.Listener --version.
	currentVersion := getRunnerVersion(runnerPath, cred, homeEnv)
	log.WithField("current_version", currentVersion).Info("Current runner version")

	// Query GitHub API for latest runner release.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.github.com/repos/actions/runner/releases/latest", nil)
	if err != nil {
		log.WithError(err).Warn("Failed to create GitHub API request")
		return
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.WithError(err).Warn("Failed to query GitHub releases API")
		globalWarmupLogs.Add("[warn] failed to query GitHub releases: " + err.Error())
		return
	}
	defer resp.Body.Close()

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		log.WithError(err).Warn("Failed to parse GitHub releases response")
		return
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	log.WithFields(logrus.Fields{
		"current": currentVersion,
		"latest":  latestVersion,
	}).Info("Runner version check")

	if currentVersion == latestVersion {
		log.Info("Runner is already at latest version, no update needed")
		globalWarmupLogs.Add("[done] runner already at latest version " + latestVersion)
		return
	}

	// Download the latest runner tarball.
	tarballURL := fmt.Sprintf(
		"https://github.com/actions/runner/releases/download/v%s/actions-runner-linux-x64-%s.tar.gz",
		latestVersion, latestVersion)
	log.WithField("url", tarballURL).Info("Downloading latest runner...")
	globalWarmupLogs.Add("[info] downloading runner " + latestVersion)

	dlCtx, dlCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer dlCancel()

	dlReq, err := http.NewRequestWithContext(dlCtx, "GET", tarballURL, nil)
	if err != nil {
		log.WithError(err).Warn("Failed to create download request")
		return
	}
	dlResp, err := http.DefaultClient.Do(dlReq)
	if err != nil {
		log.WithError(err).Warn("Failed to download runner tarball")
		globalWarmupLogs.Add("[warn] failed to download runner: " + err.Error())
		return
	}
	defer dlResp.Body.Close()

	if dlResp.StatusCode != 200 {
		log.WithField("status", dlResp.StatusCode).Warn("Runner download returned non-200")
		return
	}

	// Save tarball to temp file (world-readable so runner user can access it).
	tmpFile, err := os.CreateTemp("", "runner-*.tar.gz")
	if err != nil {
		log.WithError(err).Warn("Failed to create temp file")
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, dlResp.Body); err != nil {
		tmpFile.Close()
		log.WithError(err).Warn("Failed to save runner tarball")
		return
	}
	tmpFile.Close()
	os.Chmod(tmpPath, 0644)

	log.WithField("duration_ms", time.Since(start).Milliseconds()).Info("Runner tarball downloaded")

	// Extract as root over existing installation, then chown to runner user.
	// We extract as root because the temp file is owned by root and the runner
	// user may not have write access to all directories being overwritten.
	extractCmd := exec.Command("tar", "-xzf", tmpPath, "-C", runnerPath)
	if out, err := extractCmd.CombinedOutput(); err != nil {
		log.WithFields(logrus.Fields{
			"error":  err,
			"output": string(out),
		}).Warn("Failed to extract runner tarball")
		return
	}

	// Fix ownership — tar may extract as root, but runner needs to run as the runner user.
	if cred != nil && cred.Credential != nil {
		chownCmd := exec.Command("chown", "-R",
			fmt.Sprintf("%d:%d", cred.Credential.Uid, cred.Credential.Gid), runnerPath)
		chownCmd.Run()
	}

	elapsed := time.Since(start)
	newVersion := getRunnerVersion(runnerPath, cred, homeEnv)
	log.WithFields(logrus.Fields{
		"old_version": currentVersion,
		"new_version": newVersion,
		"duration_ms": elapsed.Milliseconds(),
	}).Info("Runner updated successfully")
	globalWarmupLogs.Add(fmt.Sprintf("[done] runner updated %s -> %s in %dms", currentVersion, newVersion, elapsed.Milliseconds()))
}

// getRunnerVersion returns the installed runner version by running Runner.Listener --version.
func getRunnerVersion(runnerPath string, cred *syscall.SysProcAttr, homeEnv string) string {
	listenerBin := filepath.Join(runnerPath, "bin", "Runner.Listener")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, listenerBin, "--version")
	cmd.Dir = runnerPath
	cmd.Env = append(os.Environ(), homeEnv, "DOTNET_EnableDiagnostics=0")
	if cred != nil {
		cmd.SysProcAttr = cred
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
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

// parseEnvFile parses an env file (KEY=VALUE per line, # comments, blank lines ignored).
func parseEnvFile(data []byte) map[string]string {
	env := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}
	return env
}

// getPreClonedPath returns the path to the pre-cloned repo in the snapshot.
// This is where the repo was cloned during warmup and baked into the rootfs.
func getPreClonedPath(data *MMDSData) string {
	if data == nil {
		return ""
	}

	// First check explicit config
	if data.Latest.GitCache.PreClonedPath != "" {
		return data.Latest.GitCache.PreClonedPath
	}

	// Derive from job.repo if not explicitly set
	// During warmup, repos are cloned to /workspace/{org}/{repo}
	// e.g., askscio/scio -> /workspace/scio/scio
	if data.Latest.Job.Repo != "" {
		repoPath := extractRepoDir(data.Latest.Job.Repo)
		return filepath.Join("/workspace", repoPath)
	}

	return ""
}

// getWorkspaceRepoPath returns the path where workflows expect to find the repo.
// This is typically {WorkspaceDir}/{org}/{repo} following GitHub Actions conventions.
func getWorkspaceRepoPath(data *MMDSData) string {
	if data == nil {
		return ""
	}

	workspaceDir := data.Latest.GitCache.WorkspaceDir
	if workspaceDir == "" {
		workspaceDir = "/mnt/ephemeral/workdir"
	}

	// Derive from job.repo
	if data.Latest.Job.Repo != "" {
		repoPath := extractRepoDir(data.Latest.Job.Repo)
		return filepath.Join(workspaceDir, repoPath)
	}

	return ""
}

func safePrefix(s string, n int) string {
	if len(s) == 0 {
		return "<empty>"
	}
	if len(s) <= n {
		return s + "..."
	}
	return s[:n] + "..."
}

// verifyConnectivity checks that the microVM can reach the target host before
// attempting long-running network operations like git clone. Fails fast instead
// of waiting for a 10-minute git timeout.
func verifyConnectivity(repoURL string) error {
	// Determine the host to check
	host := "github.com"
	if strings.Contains(repoURL, "://") {
		parts := strings.SplitN(repoURL, "://", 2)
		if len(parts) == 2 {
			host = strings.SplitN(parts[1], "/", 2)[0]
		}
	}

	// Check DNS resolution using getent (available on all Linux systems)
	log.WithField("host", host).Info("Checking DNS resolution...")
	if output, err := exec.Command("getent", "hosts", host).CombinedOutput(); err != nil {
		return fmt.Errorf("DNS resolution failed for %s: %s", host, strings.TrimSpace(string(output)))
	}

	// Check TCP connectivity to HTTPS port
	log.WithField("host", host).Info("Checking TCP connectivity to port 443...")
	conn, err := net.DialTimeout("tcp", host+":443", 10*time.Second)
	if err != nil {
		// Log diagnostic info
		log.WithError(err).Error("TCP connection failed")
		if routeOut, _ := exec.Command("ip", "route").Output(); len(routeOut) > 0 {
			log.WithField("routes", string(routeOut)).Debug("Current routes")
		}
		if resolvOut, _ := os.ReadFile("/etc/resolv.conf"); len(resolvOut) > 0 {
			log.WithField("resolv.conf", string(resolvOut)).Debug("DNS config")
		}
		if pingOut, _ := exec.Command("ping", "-c", "1", "-W", "3", "8.8.8.8").CombinedOutput(); len(pingOut) > 0 {
			log.WithField("ping_8888", string(pingOut)).Debug("Ping to 8.8.8.8")
		}
		return fmt.Errorf("cannot connect to %s:443: %w", host, err)
	}
	conn.Close()

	log.WithField("host", host).Info("Connectivity check passed")
	return nil
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

	// Environment: inherit current env + merge provided env
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

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
		healthURL := fmt.Sprintf("http://localhost:%d%s", sc.Port, sc.HealthPath)
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
			"registration": globalRegistrationState,
			"symlink":      globalSymlinkState,
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
			"github_token_set":  mmdsData.Latest.Job.GitHubRunnerToken != "",
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
		bazelVer, _ := exec.Command("bazel", "--version").CombinedOutput()
		goVer, _ := exec.Command("go", "version").CombinedOutput()
		runnerCheck, _ := exec.Command("ls", "-la", "/home/runner").CombinedOutput()

		// Check symlink paths
		workdirLs, _ := exec.Command("ls", "-la", "/mnt/ephemeral/workdir").CombinedOutput()
		workdirScioLs, _ := exec.Command("ls", "-la", "/mnt/ephemeral/workdir/scio").CombinedOutput()
		symlinkLs, _ := exec.Command("ls", "-la", "/mnt/ephemeral/workdir/scio/scio").CombinedOutput()
		targetLs, _ := exec.Command("ls", "-la", "/workspace/scio/scio/.git").CombinedOutput()

		// Try git status in the symlink path
		gitStatusCmd := exec.Command("git", "status")
		gitStatusCmd.Dir = "/mnt/ephemeral/workdir/scio/scio"
		gitStatus, _ := gitStatusCmd.CombinedOutput()

		// Check git config
		gitConfigCmd := exec.Command("cat", "/workspace/scio/scio/.git/config")
		gitConfig, _ := gitConfigCmd.CombinedOutput()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"mounts":            string(mounts),
			"lsblk":             string(lsblk),
			"df":                string(df),
			"bazel_version":     string(bazelVer),
			"go_version":        string(goVer),
			"runner_dir":        string(runnerCheck),
			"workdir_ls":        string(workdirLs),
			"workdir_scio_ls":   string(workdirScioLs),
			"symlink_ls":        string(symlinkLs),
			"symlink_target_ls": string(targetLs),
			"git_status":        string(gitStatus),
			"git_config":        string(gitConfig),
		})
	})

	server := &http.Server{
		Addr:    ":10500",
		Handler: mux,
	}

	// Check if :10500 is already bound (from warmup mode). If so, skip —
	// the warmup health server is already running and has the same endpoints.
	ln, err := net.Listen("tcp", ":10500")
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
