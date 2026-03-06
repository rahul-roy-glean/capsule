// bench-session measures pause and resume latency for session-backed runners.
// This is the equivalent of manta's cmd/bench_restore, adapted to our
// pause/resume model.
//
// It measures two distinct quantities per iteration:
//
//   - pause_only: time for POST /api/v1/runners/pause to return (VM frozen,
//     mem diff written/uploaded)
//   - resume_only: time for the allocate call (with session_id) to return a
//     runner_id after the runner reaches ready status
//   - resume_tti: resume_only + first exec response (time-to-interactive)
//
// Setup phase (untimed):
//  1. Allocate a fresh runner with a stable session_id.
//  2. Run a deterministic mutation command to write state.
//  3. Pause it — this becomes the fixture session.
//
// Measured loop:
//  1. POST /api/v1/runners/allocate (with session_id) — resume
//  2. Poll /api/v1/runners/status until 200 — runner ready
//  3. POST /api/v1/runners/{id}/exec sanity command — verify state + TTI
//  4. Pause runner again to reset for the next iteration
//
// Usage:
//
//	go run ./cmd/bench-session \
//	  --cp http://localhost:8080 \
//	  --mgr http://localhost:9080 \
//	  --workload-key <key> \
//	  --iterations 50 --warmup 5
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	flagCP           = flag.String("cp", "http://localhost:8080", "control-plane base URL")
	flagMgr          = flag.String("mgr", "http://localhost:9080", "firecracker-manager base URL")
	flagWorkloadKey  = flag.String("workload-key", "", "workload_key to allocate (required)")
	flagIterations   = flag.Int("iterations", 50, "number of measured iterations")
	flagWarmup       = flag.Int("warmup", 5, "warmup iterations (not recorded)")
	flagMutationCmd  = flag.String("mutation-cmd", `sh -c 'echo "resumed-ok" > /tmp/bench-session-state.txt'`, "command that writes deterministic state before the first pause")
	flagSanityCmd    = flag.String("sanity-cmd", `cat /tmp/bench-session-state.txt`, "command run after each resume to verify state and measure TTI")
	flagExpectStdout = flag.String("expect-stdout", "resumed-ok\n", "expected stdout from sanity-cmd")
	flagTimeout      = flag.Duration("timeout", 120*time.Second, "per-request HTTP timeout")
	flagReadyTimeout = flag.Duration("ready-timeout", 60*time.Second, "timeout polling for runner ready status")
	flagReadyPoll    = flag.Duration("ready-poll", 500*time.Millisecond, "interval between runner ready polls")
)

// --------------------------------------------------------------------------
// API types
// --------------------------------------------------------------------------

type allocateResp struct {
	RunnerID    string `json:"runner_id"`
	HostAddress string `json:"host_address"`
	SessionID   string `json:"session_id"`
	Resumed     bool   `json:"resumed"`
	Error       string `json:"error"`
}

type pauseResp struct {
	Success           bool   `json:"success"`
	SessionID         string `json:"session_id"`
	SnapshotSizeBytes int64  `json:"snapshot_size_bytes"`
	Layer             int    `json:"layer"`
	Error             string `json:"error"`
}

type releaseResp struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// execEvent is one NDJSON line from the manager's streaming exec response.
// The thaw-agent emits:
//
//	{"type":"stdout","data":"...\n","ts":"..."}
//	{"type":"stderr","data":"...\n","ts":"..."}
//	{"type":"exit","code":0,"ts":"..."}
type execEvent struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Code int    `json:"code"`
}

type runResult struct {
	PauseOnly  time.Duration
	ResumeOnly time.Duration
	ResumeTTI  time.Duration
}

// --------------------------------------------------------------------------
// main
// --------------------------------------------------------------------------

func main() {
	flag.Parse()

	if *flagWorkloadKey == "" {
		fmt.Fprintln(os.Stderr, "error: --workload-key is required")
		flag.Usage()
		os.Exit(1)
	}

	client := &http.Client{Timeout: *flagTimeout}
	cp := strings.TrimRight(*flagCP, "/")
	mgr := strings.TrimRight(*flagMgr, "/")

	fmt.Fprintf(os.Stderr, "bench-session config:\n")
	fmt.Fprintf(os.Stderr, "  cp=%s mgr=%s workload_key=%s\n", cp, mgr, *flagWorkloadKey)
	fmt.Fprintf(os.Stderr, "  warmup=%d iterations=%d\n", *flagWarmup, *flagIterations)
	fmt.Fprintf(os.Stderr, "  mutation_cmd=%q\n", *flagMutationCmd)
	fmt.Fprintf(os.Stderr, "  sanity_cmd=%q expect=%q\n", *flagSanityCmd, *flagExpectStdout)

	// --- Setup fixture ---
	// Allocate a fresh runner, run mutation, pause it.
	// The resulting session_id is reused for all measured iterations.
	sessionID, err := setupFixture(client, cp, mgr, *flagWorkloadKey, *flagMutationCmd, *flagSanityCmd, *flagExpectStdout, *flagReadyTimeout, *flagReadyPoll)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture setup failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "fixture session_id=%s\n", sessionID)

	failures := map[string]int{
		"resume_api":  0,
		"sanity_exec": 0,
		"pause":       0,
		"other":       0,
	}

	// --- Warmup ---
	for i := range *flagWarmup {
		_, class, err := runOnce(client, cp, mgr, *flagWorkloadKey, sessionID, *flagSanityCmd, *flagExpectStdout, *flagReadyTimeout, *flagReadyPoll)
		if err != nil {
			failures[class]++
			fmt.Fprintf(os.Stderr, "warmup [%d/%d] failed (%s): %v\n", i+1, *flagWarmup, class, err)
		} else {
			fmt.Fprintf(os.Stderr, "warmup [%d/%d] ok\n", i+1, *flagWarmup)
		}
	}

	// --- Measured iterations ---
	results := make([]runResult, 0, *flagIterations)
	for i := range *flagIterations {
		res, class, err := runOnce(client, cp, mgr, *flagWorkloadKey, sessionID, *flagSanityCmd, *flagExpectStdout, *flagReadyTimeout, *flagReadyPoll)
		if err != nil {
			failures[class]++
			fmt.Fprintf(os.Stderr, "run [%d/%d] failed (%s): %v\n", i+1, *flagIterations, class, err)
			continue
		}
		results = append(results, res)
		fmt.Fprintf(os.Stderr, "run [%d/%d] pause=%s resume=%s tti=%s\n",
			i+1, *flagIterations, res.PauseOnly, res.ResumeOnly, res.ResumeTTI)
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no successful runs")
		os.Exit(1)
	}

	pauseOnly := make([]time.Duration, 0, len(results))
	resumeOnly := make([]time.Duration, 0, len(results))
	resumeTTI := make([]time.Duration, 0, len(results))
	for _, r := range results {
		pauseOnly = append(pauseOnly, r.PauseOnly)
		resumeOnly = append(resumeOnly, r.ResumeOnly)
		resumeTTI = append(resumeTTI, r.ResumeTTI)
	}
	sort.Slice(pauseOnly, func(i, j int) bool { return pauseOnly[i] < pauseOnly[j] })
	sort.Slice(resumeOnly, func(i, j int) bool { return resumeOnly[i] < resumeOnly[j] })
	sort.Slice(resumeTTI, func(i, j int) bool { return resumeTTI[i] < resumeTTI[j] })

	fmt.Fprintf(os.Stderr, "\n--- Pause (%d successful) ---\n", len(results))
	fmt.Fprintf(os.Stderr, "min: %s\n", pauseOnly[0])
	fmt.Fprintf(os.Stderr, "p50: %s\n", percentile(pauseOnly, 0.50))
	fmt.Fprintf(os.Stderr, "p95: %s\n", percentile(pauseOnly, 0.95))
	fmt.Fprintf(os.Stderr, "p99: %s\n", percentile(pauseOnly, 0.99))
	fmt.Fprintf(os.Stderr, "max: %s\n", pauseOnly[len(pauseOnly)-1])

	fmt.Fprintf(os.Stderr, "\n--- Resume-only (allocate → ready) ---\n")
	fmt.Fprintf(os.Stderr, "min: %s\n", resumeOnly[0])
	fmt.Fprintf(os.Stderr, "p50: %s\n", percentile(resumeOnly, 0.50))
	fmt.Fprintf(os.Stderr, "p95: %s\n", percentile(resumeOnly, 0.95))
	fmt.Fprintf(os.Stderr, "p99: %s\n", percentile(resumeOnly, 0.99))
	fmt.Fprintf(os.Stderr, "max: %s\n", resumeOnly[len(resumeOnly)-1])

	fmt.Fprintf(os.Stderr, "\n--- Resume TTI (allocate → first exec) ---\n")
	fmt.Fprintf(os.Stderr, "min: %s\n", resumeTTI[0])
	fmt.Fprintf(os.Stderr, "p50: %s\n", percentile(resumeTTI, 0.50))
	fmt.Fprintf(os.Stderr, "p95: %s\n", percentile(resumeTTI, 0.95))
	fmt.Fprintf(os.Stderr, "p99: %s\n", percentile(resumeTTI, 0.99))
	fmt.Fprintf(os.Stderr, "max: %s\n", resumeTTI[len(resumeTTI)-1])

	summary := map[string]any{
		"workload_key":           *flagWorkloadKey,
		"session_id":             sessionID,
		"iterations_requested":   *flagIterations,
		"iterations_success":     len(results),
		"failures":               failures,
		"sanity_cmd":             *flagSanityCmd,
		"sanity_expected_stdout": *flagExpectStdout,
		// pause
		"pause_only_min_ns": pauseOnly[0].Nanoseconds(),
		"pause_only_p50_ns": percentile(pauseOnly, 0.50).Nanoseconds(),
		"pause_only_p95_ns": percentile(pauseOnly, 0.95).Nanoseconds(),
		"pause_only_p99_ns": percentile(pauseOnly, 0.99).Nanoseconds(),
		"pause_only_max_ns": pauseOnly[len(pauseOnly)-1].Nanoseconds(),
		// resume-only
		"resume_only_min_ns": resumeOnly[0].Nanoseconds(),
		"resume_only_p50_ns": percentile(resumeOnly, 0.50).Nanoseconds(),
		"resume_only_p95_ns": percentile(resumeOnly, 0.95).Nanoseconds(),
		"resume_only_p99_ns": percentile(resumeOnly, 0.99).Nanoseconds(),
		"resume_only_max_ns": resumeOnly[len(resumeOnly)-1].Nanoseconds(),
		// resume TTI
		"resume_tti_min_ns": resumeTTI[0].Nanoseconds(),
		"resume_tti_p50_ns": percentile(resumeTTI, 0.50).Nanoseconds(),
		"resume_tti_p95_ns": percentile(resumeTTI, 0.95).Nanoseconds(),
		"resume_tti_p99_ns": percentile(resumeTTI, 0.99).Nanoseconds(),
		"resume_tti_max_ns": resumeTTI[len(resumeTTI)-1].Nanoseconds(),
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		fmt.Fprintf(os.Stderr, "encode summary: %v\n", err)
		os.Exit(1)
	}
}

// --------------------------------------------------------------------------
// Fixture setup
// --------------------------------------------------------------------------

// setupFixture allocates a fresh runner, applies the mutation, verifies the
// sanity command, then pauses it. Returns the session_id to reuse across all
// measured iterations.
func setupFixture(client *http.Client, cp, mgr, workloadKey, mutationCmd, sanityCmd, expectStdout string, readyTimeout, readyPoll time.Duration) (string, error) {
	sessionID := fmt.Sprintf("bench-session-%d", time.Now().UnixNano())

	runnerID, err := allocateRunnerWithSession(client, cp, workloadKey, sessionID)
	if err != nil {
		return "", fmt.Errorf("allocate fixture runner: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  fixture runner_id=%s\n", runnerID)

	if err := waitReady(client, cp, runnerID, readyTimeout, readyPoll); err != nil {
		_ = releaseRunner(client, cp, runnerID)
		return "", fmt.Errorf("wait ready: %w", err)
	}

	// Apply deterministic mutation
	if err := execInRunner(client, mgr, runnerID, mutationCmd); err != nil {
		_ = releaseRunner(client, cp, runnerID)
		return "", fmt.Errorf("run mutation: %w", err)
	}

	// Sanity check before first pause
	stdout, err := execGetStdout(client, mgr, runnerID, sanityCmd)
	if err != nil {
		_ = releaseRunner(client, cp, runnerID)
		return "", fmt.Errorf("pre-pause sanity exec: %w", err)
	}
	if stdout != expectStdout {
		_ = releaseRunner(client, cp, runnerID)
		return "", fmt.Errorf("pre-pause sanity mismatch: got=%q want=%q", stdout, expectStdout)
	}

	// Pause to create the fixture session snapshot
	if _, err := pauseRunner(client, cp, runnerID); err != nil {
		_ = releaseRunner(client, cp, runnerID)
		return "", fmt.Errorf("pause fixture: %w", err)
	}

	return sessionID, nil
}

// --------------------------------------------------------------------------
// Single iteration: resume → poll ready → exec sanity → pause
// --------------------------------------------------------------------------

func runOnce(client *http.Client, cp, mgr, workloadKey, sessionID, sanityCmd, expectStdout string, readyTimeout, readyPoll time.Duration) (runResult, string, error) {
	var res runResult

	// Resume
	resumeStart := time.Now()
	runnerID, err := allocateRunnerWithSession(client, cp, workloadKey, sessionID)
	if err != nil {
		return res, "resume_api", fmt.Errorf("resume allocate: %w", err)
	}

	if err := waitReady(client, cp, runnerID, readyTimeout, readyPoll); err != nil {
		_ = releaseRunner(client, cp, runnerID)
		return res, "resume_api", fmt.Errorf("wait ready after resume: %w", err)
	}
	res.ResumeOnly = time.Since(resumeStart)

	// Sanity exec — also establishes TTI
	stdout, err := execGetStdout(client, mgr, runnerID, sanityCmd)
	if err != nil {
		_ = releaseRunner(client, cp, runnerID)
		return res, "sanity_exec", fmt.Errorf("sanity exec: %w", err)
	}
	if stdout != expectStdout {
		_ = releaseRunner(client, cp, runnerID)
		return res, "sanity_exec", fmt.Errorf("sanity stdout mismatch: got=%q want=%q", stdout, expectStdout)
	}
	res.ResumeTTI = time.Since(resumeStart)

	// Pause — measure duration, then runner is ready for the next iteration
	pauseStart := time.Now()
	if _, err := pauseRunner(client, cp, runnerID); err != nil {
		_ = releaseRunner(client, cp, runnerID)
		return res, "pause", fmt.Errorf("pause: %w", err)
	}
	res.PauseOnly = time.Since(pauseStart)

	return res, "", nil
}

// --------------------------------------------------------------------------
// API helpers
// --------------------------------------------------------------------------

func allocateRunnerWithSession(client *http.Client, cp, workloadKey, sessionID string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"workload_key": workloadKey,
		"session_id":   sessionID,
	})
	req, _ := http.NewRequest(http.MethodPost, cp+"/api/v1/runners/allocate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	raw, err := doJSON(client, req)
	if err != nil {
		return "", err
	}
	var resp allocateResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode allocate response: %w (body=%q)", err, strings.TrimSpace(string(raw)))
	}
	if resp.Error != "" || resp.RunnerID == "" {
		return "", fmt.Errorf("allocate failed: error=%q runner_id=%q", resp.Error, resp.RunnerID)
	}
	return resp.RunnerID, nil
}

func waitReady(client *http.Client, cp, runnerID string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(cp + "/api/v1/runners/status?runner_id=" + runnerID)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(poll)
	}
	return fmt.Errorf("runner %s not ready after %s", runnerID, timeout)
}

func execInRunner(client *http.Client, mgr, runnerID, cmd string) error {
	_, err := execGetStdout(client, mgr, runnerID, cmd)
	return err
}

// execGetStdout runs cmd in the runner and collects stdout from the
// streaming NDJSON exec response used by the manager's /exec endpoint.
// Retries on transient connection errors (thaw-agent readiness race).
func execGetStdout(client *http.Client, mgr, runnerID, cmd string) (string, error) {
	bodyBytes, _ := json.Marshal(map[string]any{
		"command":         []string{"sh", "-c", cmd},
		"timeout_seconds": 30,
	})
	url := mgr + "/api/v1/runners/" + runnerID + "/exec"
	const maxRetries = 5
	const retryDelay = 300 * time.Millisecond

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			if strings.Contains(err.Error(), "connection reset by peer") ||
				strings.Contains(err.Error(), "EOF") ||
				strings.Contains(err.Error(), "connection refused") {
				lastErr = err
				continue
			}
			return "", err
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			buf := new(bytes.Buffer)
			buf.ReadFrom(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("exec status %d body=%s", resp.StatusCode, strings.TrimSpace(buf.String()))
			continue
		}

		// The manager streams NDJSON exec events. Collect stdout lines and check
		// for a non-zero exit code.
		var stdout strings.Builder
		dec := json.NewDecoder(resp.Body)
		for dec.More() {
			var ev execEvent
			if err := dec.Decode(&ev); err != nil {
				break
			}
			switch ev.Type {
			case "stdout":
				stdout.WriteString(ev.Data)
			case "exit":
				if ev.Code != 0 {
					resp.Body.Close()
					return "", fmt.Errorf("exec exited with code %d", ev.Code)
				}
			}
		}
		resp.Body.Close()
		return stdout.String(), nil
	}
	return "", lastErr
}

func pauseRunner(client *http.Client, cp, runnerID string) (pauseResp, error) {
	body, _ := json.Marshal(map[string]string{"runner_id": runnerID})
	req, _ := http.NewRequest(http.MethodPost, cp+"/api/v1/runners/pause", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	raw, err := doJSON(client, req)
	if err != nil {
		return pauseResp{}, err
	}
	var resp pauseResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return pauseResp{}, fmt.Errorf("decode pause response: %w (body=%q)", err, strings.TrimSpace(string(raw)))
	}
	if resp.Error != "" {
		return pauseResp{}, errors.New(resp.Error)
	}
	return resp, nil
}

func releaseRunner(client *http.Client, cp, runnerID string) error {
	body, _ := json.Marshal(map[string]string{"runner_id": runnerID})
	req, _ := http.NewRequest(http.MethodPost, cp+"/api/v1/runners/release", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	raw, err := doJSON(client, req)
	if err != nil {
		return err
	}
	var resp releaseResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode release response: %w (body=%q)", err, strings.TrimSpace(string(raw)))
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

func doJSON(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d body=%s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	return buf.Bytes(), nil
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	idx := int(float64(len(values)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}
