// Package cpapi provides a Go client for the control plane HTTP API.
// It handles JSON serialization, 429 capacity-retry with exponential backoff,
// and transient error recovery.
package cpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Client talks to the control plane HTTP API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	// MaxRetries is the maximum number of retries on 429/capacity errors.
	// Defaults to 5 if zero.
	MaxRetries int
	// InitialBackoff is the starting backoff duration for retries.
	// Defaults to 2s if zero.
	InitialBackoff time.Duration
}

// AllocateRequest is the request body for POST /api/v1/runners/allocate.
type AllocateRequest struct {
	RequestID           string            `json:"request_id,omitempty"`
	WorkloadKey         string            `json:"workload_key"`
	Labels              map[string]string `json:"labels,omitempty"`
	SessionID           string            `json:"session_id,omitempty"`
	NetworkPolicyPreset string            `json:"network_policy_preset,omitempty"`
	NetworkPolicyJSON   string            `json:"network_policy_json,omitempty"`
}

// AllocateResponse is the response body from POST /api/v1/runners/allocate.
type AllocateResponse struct {
	RunnerID    string `json:"runner_id"`
	HostID      string `json:"host_id"`
	HostAddress string `json:"host_address"`
	InternalIP  string `json:"internal_ip"`
	SessionID   string `json:"session_id"`
	Resumed     bool   `json:"resumed"`
	Error       string `json:"error"`
}

// ReleaseResponse is the response body from POST /api/v1/runners/release.
type ReleaseResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// PauseResponse is the response body from POST /api/v1/runners/pause.
type PauseResponse struct {
	Success           bool   `json:"success"`
	SessionID         string `json:"session_id"`
	SnapshotSizeBytes int64  `json:"snapshot_size_bytes"`
	Layer             int    `json:"layer"`
	Error             string `json:"error"`
}

func (c *Client) maxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return 5
}

func (c *Client) initialBackoff() time.Duration {
	if c.InitialBackoff > 0 {
		return c.InitialBackoff
	}
	return 2 * time.Second
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// AllocateRunner allocates a runner. Automatically retries on 429 (capacity)
// responses using the Retry-After header with exponential backoff.
func (c *Client) AllocateRunner(req AllocateRequest) (*AllocateResponse, error) {
	var resp AllocateResponse
	if err := c.doWithRetry(http.MethodPost, "/api/v1/runners/allocate", req, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("allocate failed: %s", resp.Error)
	}
	if resp.RunnerID == "" {
		return nil, fmt.Errorf("allocate returned empty runner_id")
	}
	return &resp, nil
}

// ReleaseRunner releases (destroys) a runner.
func (c *Client) ReleaseRunner(runnerID string) error {
	var resp ReleaseResponse
	if err := c.doWithRetry(http.MethodPost, "/api/v1/runners/release", map[string]string{"runner_id": runnerID}, &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("release failed: %s", resp.Error)
	}
	return nil
}

// PauseRunner pauses a runner and creates a session snapshot.
func (c *Client) PauseRunner(runnerID string) (*PauseResponse, error) {
	var resp PauseResponse
	if err := c.doWithRetry(http.MethodPost, "/api/v1/runners/pause", map[string]string{"runner_id": runnerID}, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("pause failed: %s", resp.Error)
	}
	return &resp, nil
}

// WaitReady polls the runner status endpoint until the runner is ready or the
// timeout expires.
func (c *Client) WaitReady(runnerID string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := c.httpClient().Get(c.BaseURL + "/api/v1/runners/status?runner_id=" + runnerID)
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

// doWithRetry sends a JSON request and retries on 429 responses.
func (c *Client) doWithRetry(method, path string, reqBody any, result any) error {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	backoff := c.initialBackoff()
	maxRetries := c.maxRetries()
	requestID := uuid.New().String()

	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest(method, c.BaseURL+path, bytes.NewReader(bodyBytes))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", requestID)

		resp, err := c.httpClient().Do(req)
		if err != nil {
			return err
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read response: %w", readErr)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			delay := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					delay = time.Duration(secs) * time.Second
				}
			}
			time.Sleep(delay)
			backoff = time.Duration(float64(backoff) * math.Min(2.0, 1.5))
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}

		if result != nil {
			if err := json.Unmarshal(respBody, result); err != nil {
				return fmt.Errorf("decode response: %w (body=%q)", err, strings.TrimSpace(string(respBody)))
			}
		}
		return nil
	}
	return fmt.Errorf("max retries (%d) exhausted for %s %s", maxRetries, method, path)
}
