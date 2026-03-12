package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// execResult is the aggregated output of an exec command.
type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// proxyExecToHost sends an exec request to the host agent and buffers the NDJSON
// stream into a single execResult. The host agent streams output lines as NDJSON
// objects; we collect them until the stream closes or the timeout fires.
func proxyExecToHost(ctx context.Context, hostHTTP, runnerID, command, workingDir string, env map[string]string, timeoutSecs int) (*execResult, error) {
	if timeoutSecs <= 0 {
		timeoutSecs = 300
	}

	body, err := json.Marshal(map[string]any{
		"command":     command,
		"working_dir": workingDir,
		"env":         env,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal exec body: %w", err)
	}

	u := url.URL{
		Scheme: "http",
		Host:   hostHTTP,
		Path:   fmt.Sprintf("/api/v1/runners/%s/exec", runnerID),
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(execCtx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create exec request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exec request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("exec returned HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	// Buffer NDJSON lines from host agent.
	// Each line is {"stream":"stdout"|"stderr","data":"..."} or {"exit_code":N}
	var stdout, stderr strings.Builder
	exitCode := -1
	decoder := json.NewDecoder(resp.Body)
	for decoder.More() {
		var line map[string]any
		if err := decoder.Decode(&line); err != nil {
			break
		}
		if s, ok := line["stream"].(string); ok {
			data, _ := line["data"].(string)
			switch s {
			case "stdout":
				stdout.WriteString(data)
			case "stderr":
				stderr.WriteString(data)
			}
		}
		if code, ok := line["exit_code"].(float64); ok {
			exitCode = int(code)
		}
	}

	return &execResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// proxyFileReadToHost reads a file from a runner via the host agent.
func proxyFileReadToHost(ctx context.Context, hostHTTP, runnerID, path string, offset, limit int) (string, error) {
	u := url.URL{
		Scheme: "http",
		Host:   hostHTTP,
		Path:   fmt.Sprintf("/api/v1/runners/%s/files/read", runnerID),
	}
	q := u.Query()
	q.Set("path", path)
	if offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", offset))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("file read request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10MB limit
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("file read returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// proxyFileWriteToHost writes a file to a runner via the host agent.
func proxyFileWriteToHost(ctx context.Context, hostHTTP, runnerID, path, content, mode string) error {
	body, err := json.Marshal(map[string]string{
		"path":    path,
		"content": content,
		"mode":    mode,
	})
	if err != nil {
		return err
	}

	u := url.URL{
		Scheme: "http",
		Host:   hostHTTP,
		Path:   fmt.Sprintf("/api/v1/runners/%s/files/write", runnerID),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("file write request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("file write returned HTTP %d: %s", resp.StatusCode, string(errBody))
	}
	return nil
}

// proxyFileListToHost lists files in a directory on a runner via the host agent.
func proxyFileListToHost(ctx context.Context, hostHTTP, runnerID, path string) (json.RawMessage, error) {
	u := url.URL{
		Scheme: "http",
		Host:   hostHTTP,
		Path:   fmt.Sprintf("/api/v1/runners/%s/files/list", runnerID),
	}
	q := u.Query()
	q.Set("path", path)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("file list request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("file list returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.RawMessage(body), nil
}
