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
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rahul-roy-glean/capsule/pkg/cpapi"
)

var (
	flagCP           = flag.String("cp", "http://localhost:8080", "control-plane base URL")
	flagMgr          = flag.String("mgr", "http://localhost:9080", "capsule-manager base URL")
	flagWorkloadKey  = flag.String("workload-key", "", "workload_key to allocate (required)")
	flagIterations   = flag.Int("iterations", 50, "number of measured iterations")
	flagWarmup       = flag.Int("warmup", 5, "warmup iterations (not recorded)")
	flagCmd          = flag.String("cmd", `echo "benchmark"`, "shell command to run inside the VM via exec")
	flagTimeout      = flag.Duration("timeout", 120*time.Second, "per-request HTTP timeout")
	flagReadyTimeout = flag.Duration("ready-timeout", 60*time.Second, "timeout polling for runner ready status")
	flagReadyPoll    = flag.Duration("ready-poll", 500*time.Millisecond, "interval between runner ready polls")
)

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

	cp := &cpapi.Client{
		BaseURL:    strings.TrimRight(*flagCP, "/"),
		HTTPClient: &http.Client{Timeout: *flagTimeout},
	}
	mgr := strings.TrimRight(*flagMgr, "/")
	mgrClient := &http.Client{Timeout: *flagTimeout}

	fmt.Fprintf(os.Stderr, "bench-allocate config:\n")
	fmt.Fprintf(os.Stderr, "  cp=%s mgr=%s workload_key=%s\n", cp.BaseURL, mgr, *flagWorkloadKey)
	fmt.Fprintf(os.Stderr, "  warmup=%d iterations=%d cmd=%q\n", *flagWarmup, *flagIterations, *flagCmd)

	for i := range *flagWarmup {
		if _, err := runOnce(cp, mgrClient, mgr, *flagWorkloadKey, *flagCmd, *flagReadyTimeout, *flagReadyPoll); err != nil {
			fmt.Fprintf(os.Stderr, "warmup [%d/%d] failed: %v\n", i+1, *flagWarmup, err)
		} else {
			fmt.Fprintf(os.Stderr, "warmup [%d/%d] ok\n", i+1, *flagWarmup)
		}
	}

	results := make([]runResult, 0, *flagIterations)
	for i := range *flagIterations {
		res, err := runOnce(cp, mgrClient, mgr, *flagWorkloadKey, *flagCmd, *flagReadyTimeout, *flagReadyPoll)
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
func runOnce(cp *cpapi.Client, mgrClient *http.Client, mgr, workloadKey, cmd string, readyTimeout, readyPoll time.Duration) (runResult, error) {
	start := time.Now()

	resp, err := cp.AllocateRunner(cpapi.AllocateRequest{WorkloadKey: workloadKey})
	if err != nil {
		return runResult{}, fmt.Errorf("allocate: %w", err)
	}
	runnerID := resp.RunnerID

	release := func() {
		if rErr := cp.ReleaseRunner(runnerID); rErr != nil {
			fmt.Fprintf(os.Stderr, "  release %s failed: %v\n", runnerID, rErr)
		}
	}

	if err := cp.WaitReady(runnerID, readyTimeout, readyPoll); err != nil {
		release()
		return runResult{}, fmt.Errorf("wait ready: %w", err)
	}

	if err := execInRunner(mgrClient, mgr, runnerID, cmd); err != nil {
		release()
		return runResult{}, fmt.Errorf("exec: %w", err)
	}

	elapsed := time.Since(start)
	release()
	return runResult{Duration: elapsed}, nil
}

func execInRunner(client *http.Client, mgr, runnerID, cmd string) error {
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
			return err
		}
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("exec status %d body=%s", resp.StatusCode, strings.TrimSpace(buf.String()))
		}
		return nil
	}
	return lastErr
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
