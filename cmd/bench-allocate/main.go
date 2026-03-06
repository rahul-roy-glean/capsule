// bench-allocate measures the end-to-end latency of allocating a fresh runner,
// running a command inside it, and releasing it. This is the "cold path"
// equivalent of manta's cmd/bench.
//
// It talks to both the control plane (allocate/release) and the manager
// HTTP API (exec) exactly as the dev E2E tests do.
//
// Usage:
//
//	go run ./cmd/bench-allocate \
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
	flagCmd          = flag.String("cmd", `echo "benchmark"`, "shell command to run inside the VM via exec")
	flagTimeout      = flag.Duration("timeout", 120*time.Second, "per-request HTTP timeout")
	flagReadyTimeout = flag.Duration("ready-timeout", 60*time.Second, "timeout polling for runner ready status")
	flagReadyPoll    = flag.Duration("ready-poll", 500*time.Millisecond, "interval between runner ready polls")
)

type allocateResp struct {
	RunnerID    string `json:"runner_id"`
	HostAddress string `json:"host_address"`
	SessionID   string `json:"session_id"`
	Resumed     bool   `json:"resumed"`
	Error       string `json:"error"`
}

type releaseResp struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type runResult struct {
	Duration time.Duration
}

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

	fmt.Fprintf(os.Stderr, "bench-allocate config:\n")
	fmt.Fprintf(os.Stderr, "  cp=%s mgr=%s workload_key=%s\n", cp, mgr, *flagWorkloadKey)
	fmt.Fprintf(os.Stderr, "  warmup=%d iterations=%d cmd=%q\n", *flagWarmup, *flagIterations, *flagCmd)

	for i := range *flagWarmup {
		if _, err := runOnce(client, cp, mgr, *flagWorkloadKey, *flagCmd, *flagReadyTimeout, *flagReadyPoll); err != nil {
			fmt.Fprintf(os.Stderr, "warmup [%d/%d] failed: %v\n", i+1, *flagWarmup, err)
		} else {
			fmt.Fprintf(os.Stderr, "warmup [%d/%d] ok\n", i+1, *flagWarmup)
		}
	}

	results := make([]runResult, 0, *flagIterations)
	for i := range *flagIterations {
		res, err := runOnce(client, cp, mgr, *flagWorkloadKey, *flagCmd, *flagReadyTimeout, *flagReadyPoll)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run [%d/%d] failed: %v\n", i+1, *flagIterations, err)
			continue
		}
		results = append(results, res)
		fmt.Fprintf(os.Stderr, "run [%d/%d] %s\n", i+1, *flagIterations, res.Duration)
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no successful runs")
		os.Exit(1)
	}

	durations := make([]time.Duration, 0, len(results))
	for _, r := range results {
		durations = append(durations, r.Duration)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	fmt.Fprintf(os.Stderr, "\n--- Results (%d successful / %d requested) ---\n", len(durations), *flagIterations)
	fmt.Fprintf(os.Stderr, "min: %s\n", durations[0])
	fmt.Fprintf(os.Stderr, "p50: %s\n", percentile(durations, 0.50))
	fmt.Fprintf(os.Stderr, "p95: %s\n", percentile(durations, 0.95))
	fmt.Fprintf(os.Stderr, "p99: %s\n", percentile(durations, 0.99))
	fmt.Fprintf(os.Stderr, "max: %s\n", durations[len(durations)-1])

	summary := map[string]any{
		"workload_key":         *flagWorkloadKey,
		"iterations_requested": *flagIterations,
		"iterations_success":   len(durations),
		"min_ns":               durations[0].Nanoseconds(),
		"p50_ns":               percentile(durations, 0.50).Nanoseconds(),
		"p95_ns":               percentile(durations, 0.95).Nanoseconds(),
		"p99_ns":               percentile(durations, 0.99).Nanoseconds(),
		"max_ns":               durations[len(durations)-1].Nanoseconds(),
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		fmt.Fprintf(os.Stderr, "encode summary: %v\n", err)
		os.Exit(1)
	}
}

// runOnce performs one full cycle: allocate → poll ready → exec → release.
// The timer runs from allocate request to end of exec response; release is untimed.
func runOnce(client *http.Client, cp, mgr, workloadKey, cmd string, readyTimeout, readyPoll time.Duration) (runResult, error) {
	start := time.Now()

	runnerID, err := allocateRunner(client, cp, workloadKey)
	if err != nil {
		return runResult{}, fmt.Errorf("allocate: %w", err)
	}

	release := func() {
		if rErr := releaseRunner(client, cp, runnerID); rErr != nil {
			fmt.Fprintf(os.Stderr, "  release %s failed: %v\n", runnerID, rErr)
		}
	}

	if err := waitReady(client, cp, runnerID, readyTimeout, readyPoll); err != nil {
		release()
		return runResult{}, fmt.Errorf("wait ready: %w", err)
	}

	if err := execInRunner(client, mgr, runnerID, cmd); err != nil {
		release()
		return runResult{}, fmt.Errorf("exec: %w", err)
	}

	elapsed := time.Since(start)
	release()
	return runResult{Duration: elapsed}, nil
}

func allocateRunner(client *http.Client, cp, workloadKey string) (string, error) {
	body, _ := json.Marshal(map[string]string{"workload_key": workloadKey})
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
	body, _ := json.Marshal(map[string]any{
		"command":         []string{"sh", "-c", cmd},
		"timeout_seconds": 30,
	})
	req, _ := http.NewRequest(http.MethodPost, mgr+"/api/v1/runners/"+runnerID+"/exec", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	_, err := doJSON(client, req)
	return err
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
