package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

// WaitForThawAgentExec polls the capsule-thaw-agent by sending a trivial exec
// command until it responds successfully. This is more reliable than checking
// /alive after snapshot restore because /alive can respond before the exec
// handler is fully functional.
func WaitForThawAgentExec(ctx context.Context, ip string, timeout time.Duration) error {
	execURL := fmt.Sprintf("http://%s:%d/exec", ip, snapshot.ThawAgentDebugPort)
	return waitForThawAgentExecURL(ctx, execURL, timeout)
}

func waitForThawAgentExecURL(ctx context.Context, execURL string, timeout time.Duration) error {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	body := []byte(`{"command":["echo","ready"],"timeout_seconds":3}`)

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, execURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create thaw-agent exec request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err == nil {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(respBody), "ready") {
				return nil
			}
		}

		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("capsule-thaw-agent at %s not ready after %s", execURL, timeout)
}
