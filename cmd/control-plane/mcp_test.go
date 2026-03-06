package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
)

// newTestMCPDeps creates an mcpDeps with an in-memory HostRegistry and no DB.
func newTestMCPDeps() *mcpDeps {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)
	return &mcpDeps{
		scheduler:    &Scheduler{hostRegistry: hr, logger: logger.WithField("test", true)},
		hostRegistry: hr,
		db:           nil,
		logger:       logger.WithField("component", "mcp-test"),
	}
}

// addTestHost inserts a host into the in-memory registry.
func addTestHost(hr *HostRegistry, id, name, grpcAddr string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	hr.hosts[id] = &Host{
		ID:                 id,
		InstanceName:       name,
		Zone:               "us-central1-a",
		Status:             "ready",
		GRPCAddress:        grpcAddr,
		HTTPAddress:        "10.0.0.1:8080",
		LastHeartbeat:      time.Now(),
		TotalCPUMillicores: 16000,
		UsedCPUMillicores:  4000,
		TotalMemoryMB:      65536,
		UsedMemoryMB:       16384,
		IdleRunners:        2,
		BusyRunners:        1,
		RunnerInfos: []HostRunnerInfo{
			{RunnerID: "runner-1", State: "idle", WorkloadKey: "wk-abc"},
			{RunnerID: "runner-2", State: "busy", WorkloadKey: "wk-xyz"},
		},
	}
}

// addTestRunner inserts a runner into the in-memory registry.
func addTestRunner(hr *HostRegistry, id, hostID, status, workloadKey string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	hr.runners[id] = &Runner{
		ID:          id,
		HostID:      hostID,
		Status:      status,
		InternalIP:  "172.16.0.10",
		WorkloadKey: workloadKey,
	}
}

// --- Unit tests for tool handlers ---

func TestHandleListHosts_Empty(t *testing.T) {
	deps := newTestMCPDeps()
	ctx := context.Background()
	_, out, err := deps.handleListHosts(ctx, nil, ListHostsInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 0 {
		t.Fatalf("expected 0 hosts, got %d", out.Count)
	}
	if len(out.Hosts) != 0 {
		t.Fatalf("expected empty hosts slice, got %d", len(out.Hosts))
	}
}

func TestHandleListHosts_WithHosts(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")
	addTestHost(deps.hostRegistry, "host-2", "instance-2", "10.0.0.2:50051")

	ctx := context.Background()
	_, out, err := deps.handleListHosts(ctx, nil, ListHostsInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 2 {
		t.Fatalf("expected 2 hosts, got %d", out.Count)
	}
}

func TestHandleListSandboxes_NoFilter(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")

	ctx := context.Background()
	_, out, err := deps.handleListSandboxes(ctx, nil, ListSandboxesInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 2 {
		t.Fatalf("expected 2 sandboxes, got %d", out.Count)
	}
}

func TestHandleListSandboxes_FilterByWorkloadKey(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")

	ctx := context.Background()
	_, out, err := deps.handleListSandboxes(ctx, nil, ListSandboxesInput{WorkloadKey: "wk-abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 1 {
		t.Fatalf("expected 1 sandbox, got %d", out.Count)
	}
	if out.Sandboxes[0].WorkloadKey != "wk-abc" {
		t.Fatalf("expected workload_key wk-abc, got %s", out.Sandboxes[0].WorkloadKey)
	}
}

func TestHandleListSandboxes_FilterByStatus(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")

	ctx := context.Background()
	_, out, err := deps.handleListSandboxes(ctx, nil, ListSandboxesInput{Status: "busy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 1 {
		t.Fatalf("expected 1 sandbox, got %d", out.Count)
	}
	if out.Sandboxes[0].SandboxID != "runner-2" {
		t.Fatalf("expected runner-2, got %s", out.Sandboxes[0].SandboxID)
	}
}

func TestHandleListSandboxes_NoMatch(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")

	ctx := context.Background()
	_, out, err := deps.handleListSandboxes(ctx, nil, ListSandboxesInput{WorkloadKey: "nonexistent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 0 {
		t.Fatalf("expected 0 sandboxes, got %d", out.Count)
	}
}

func TestHandleGetStatus_RunnerExists(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")
	addTestRunner(deps.hostRegistry, "runner-1", "host-1", "busy", "wk-abc")

	ctx := context.Background()
	_, out, err := deps.handleGetStatus(ctx, nil, GetSandboxStatusInput{SandboxID: "runner-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "busy" {
		t.Fatalf("expected status 'busy', got %q", out.Status)
	}
	if out.HostID != "host-1" {
		t.Fatalf("expected host_id 'host-1', got %q", out.HostID)
	}
	if out.HostAddress != "10.0.0.1:8080" {
		t.Fatalf("expected host_address '10.0.0.1:8080', got %q", out.HostAddress)
	}
}

func TestHandleGetStatus_NotFound(t *testing.T) {
	deps := newTestMCPDeps()

	ctx := context.Background()
	_, _, err := deps.handleGetStatus(ctx, nil, GetSandboxStatusInput{SandboxID: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent sandbox")
	}
	if !strings.Contains(err.Error(), "sandbox not found") {
		t.Fatalf("expected 'sandbox not found' error, got: %v", err)
	}
}

func TestHandleRelease_NotFound(t *testing.T) {
	deps := newTestMCPDeps()

	ctx := context.Background()
	_, _, err := deps.handleRelease(ctx, nil, ReleaseSandboxInput{SandboxID: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent sandbox")
	}
}

func TestHandlePause_RunnerNotFound(t *testing.T) {
	deps := newTestMCPDeps()

	ctx := context.Background()
	_, _, err := deps.handlePause(ctx, nil, PauseSandboxInput{SandboxID: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent sandbox")
	}
	if !strings.Contains(err.Error(), "runner not found") {
		t.Fatalf("expected 'runner not found' error, got: %v", err)
	}
}

func TestHandleResume_AlreadyRunning(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")
	addTestRunner(deps.hostRegistry, "runner-1", "host-1", "busy", "wk-abc")

	ctx := context.Background()
	_, _, err := deps.handleResume(ctx, nil, ResumeSandboxInput{SandboxID: "runner-1"})
	if err == nil {
		t.Fatal("expected error for already-running sandbox")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected 'already running' error, got: %v", err)
	}
}

func TestHandleResume_NoDB(t *testing.T) {
	deps := newTestMCPDeps()
	// No runner registered, no DB → should fail at DB lookup
	ctx := context.Background()
	_, _, err := deps.handleResume(ctx, nil, ResumeSandboxInput{SandboxID: "runner-xyz"})
	if err == nil {
		t.Fatal("expected error with nil DB")
	}
	if !strings.Contains(err.Error(), "no database configured") {
		t.Fatalf("expected 'no database configured' error, got: %v", err)
	}
}

func TestHandleExec_RunnerNotFound(t *testing.T) {
	deps := newTestMCPDeps()

	ctx := context.Background()
	_, _, err := deps.handleExec(ctx, nil, ExecCommandInput{SandboxID: "nonexistent", Command: "echo hi"})
	if err == nil {
		t.Fatal("expected error for nonexistent sandbox")
	}
	if !strings.Contains(err.Error(), "sandbox not found") {
		t.Fatalf("expected 'sandbox not found' error, got: %v", err)
	}
}

func TestHandleReadFile_RunnerNotFound(t *testing.T) {
	deps := newTestMCPDeps()

	ctx := context.Background()
	_, _, err := deps.handleReadFile(ctx, nil, ReadFileInput{SandboxID: "nonexistent", Path: "/etc/hosts"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHandleWriteFile_RunnerNotFound(t *testing.T) {
	deps := newTestMCPDeps()

	ctx := context.Background()
	_, _, err := deps.handleWriteFile(ctx, nil, WriteFileInput{SandboxID: "nonexistent", Path: "/tmp/test", Content: "hello"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHandleListFiles_RunnerNotFound(t *testing.T) {
	deps := newTestMCPDeps()

	ctx := context.Background()
	_, _, err := deps.handleListFiles(ctx, nil, ListFilesInput{SandboxID: "nonexistent", Path: "/tmp"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveHostHTTP(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")
	addTestRunner(deps.hostRegistry, "runner-1", "host-1", "busy", "wk-abc")

	addr, err := deps.resolveHostHTTP("runner-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// hostAgentHTTPAddress replaces port with 8080
	if addr != "10.0.0.1:8080" {
		t.Fatalf("expected 10.0.0.1:8080, got %s", addr)
	}
}

func TestResolveHostHTTP_NotFound(t *testing.T) {
	deps := newTestMCPDeps()
	_, err := deps.resolveHostHTTP("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- MCP integration tests via in-memory transport ---

func TestMCPServer_ListTools(t *testing.T) {
	deps := newTestMCPDeps()
	server := createTestMCPServer(deps)

	ctx := context.Background()
	session := connectTestClient(t, ctx, server)
	defer session.Close()

	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("error listing tools: %v", err)
		}
		tools = append(tools, tool)
	}

	if len(tools) != 11 {
		t.Fatalf("expected 11 tools, got %d", len(tools))
	}

	// Verify expected tool names
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}

	expectedNames := []string{
		"allocate_sandbox", "release_sandbox", "pause_sandbox", "resume_sandbox",
		"get_sandbox_status", "list_sandboxes", "list_hosts",
		"exec_command", "read_file", "write_file", "list_files",
	}
	for _, name := range expectedNames {
		if !names[name] {
			t.Errorf("missing tool: %s", name)
		}
	}
}

func TestMCPServer_CallListHosts(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")

	server := createTestMCPServer(deps)
	ctx := context.Background()
	session := connectTestClient(t, ctx, server)
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_hosts",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error")
	}

	// The structured output should contain our host
	out, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected map structured content, got %T", result.StructuredContent)
	}
	count, _ := out["count"].(float64)
	if int(count) != 1 {
		t.Fatalf("expected count=1, got %v", out["count"])
	}
}

func TestMCPServer_CallListSandboxes(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")

	server := createTestMCPServer(deps)
	ctx := context.Background()
	session := connectTestClient(t, ctx, server)
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_sandboxes",
		Arguments: map[string]any{"workload_key": "wk-abc"},
	})
	if err != nil {
		t.Fatalf("CallTool error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error")
	}

	out, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected map structured content, got %T", result.StructuredContent)
	}
	count, _ := out["count"].(float64)
	if int(count) != 1 {
		t.Fatalf("expected count=1 for wk-abc, got %v", out["count"])
	}
}

func TestMCPServer_CallGetStatus_NotFound(t *testing.T) {
	deps := newTestMCPDeps()
	server := createTestMCPServer(deps)
	ctx := context.Background()
	session := connectTestClient(t, ctx, server)
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_sandbox_status",
		Arguments: map[string]any{"sandbox_id": "nonexistent"},
	})
	if err != nil {
		t.Fatalf("CallTool error: %v", err)
	}
	// Tool errors are returned as IsError=true content, not Go errors
	if !result.IsError {
		t.Fatal("expected tool error for nonexistent sandbox")
	}
}

func TestMCPServer_CallGetStatus_Found(t *testing.T) {
	deps := newTestMCPDeps()
	addTestHost(deps.hostRegistry, "host-1", "instance-1", "10.0.0.1:50051")
	addTestRunner(deps.hostRegistry, "runner-1", "host-1", "busy", "wk-abc")

	server := createTestMCPServer(deps)
	ctx := context.Background()
	session := connectTestClient(t, ctx, server)
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_sandbox_status",
		Arguments: map[string]any{"sandbox_id": "runner-1"},
	})
	if err != nil {
		t.Fatalf("CallTool error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected tool error")
	}

	out, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result.StructuredContent)
	}
	if out["status"] != "busy" {
		t.Fatalf("expected status=busy, got %v", out["status"])
	}
}

// --- HTTP-level integration test ---

func TestMCPHandler_HTTPInitialize(t *testing.T) {
	deps := newTestMCPDeps()
	handler := newMCPHandler(deps, "")

	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}
}

func TestMCPHandler_HTTPWithAuth_ValidToken(t *testing.T) {
	deps := newTestMCPDeps()
	handler := newMCPHandler(deps, "test-secret-token")

	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer test-secret-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}
}

func TestMCPHandler_HTTPWithAuth_InvalidToken(t *testing.T) {
	deps := newTestMCPDeps()
	handler := newMCPHandler(deps, "test-secret-token")

	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer wrong-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestMCPHandler_HTTPWithAuth_NoToken(t *testing.T) {
	deps := newTestMCPDeps()
	handler := newMCPHandler(deps, "test-secret-token")

	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// No Authorization header

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// --- Exec proxy test with mock host agent ---

func TestProxyExecToHost(t *testing.T) {
	// Spin up a fake host agent that returns NDJSON
	fakeHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		lines := []string{
			`{"stream":"stdout","data":"hello world\n"}`,
			`{"stream":"stderr","data":"some warning\n"}`,
			`{"exit_code":0}`,
		}
		for _, line := range lines {
			w.Write([]byte(line + "\n"))
		}
	}))
	defer fakeHost.Close()

	// Extract host:port from the test server URL
	hostAddr := strings.TrimPrefix(fakeHost.URL, "http://")

	result, err := proxyExecToHost(context.Background(), hostAddr, "runner-1", "echo hello", "", nil, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit_code=0, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello world") {
		t.Fatalf("expected stdout to contain 'hello world', got %q", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "some warning") {
		t.Fatalf("expected stderr to contain 'some warning', got %q", result.Stderr)
	}
}

func TestProxyExecToHost_Error(t *testing.T) {
	fakeHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "runner not found", http.StatusNotFound)
	}))
	defer fakeHost.Close()

	hostAddr := strings.TrimPrefix(fakeHost.URL, "http://")
	_, err := proxyExecToHost(context.Background(), hostAddr, "runner-1", "echo hello", "", nil, 10)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 error, got: %v", err)
	}
}

func TestProxyFileReadToHost(t *testing.T) {
	fakeHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("file content here"))
	}))
	defer fakeHost.Close()

	hostAddr := strings.TrimPrefix(fakeHost.URL, "http://")
	content, err := proxyFileReadToHost(context.Background(), hostAddr, "runner-1", "/etc/hosts", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "file content here" {
		t.Fatalf("expected 'file content here', got %q", content)
	}
}

func TestProxyFileWriteToHost(t *testing.T) {
	var receivedBody []byte
	fakeHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeHost.Close()

	hostAddr := strings.TrimPrefix(fakeHost.URL, "http://")
	err := proxyFileWriteToHost(context.Background(), hostAddr, "runner-1", "/tmp/test.txt", "hello", "0644")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	json.Unmarshal(receivedBody, &parsed)
	if parsed["path"] != "/tmp/test.txt" {
		t.Fatalf("expected path=/tmp/test.txt, got %q", parsed["path"])
	}
	if parsed["content"] != "hello" {
		t.Fatalf("expected content=hello, got %q", parsed["content"])
	}
}

func TestProxyFileListToHost(t *testing.T) {
	fakeHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"name":"file1.txt","size":100},{"name":"dir1","is_dir":true}]`))
	}))
	defer fakeHost.Close()

	hostAddr := strings.TrimPrefix(fakeHost.URL, "http://")
	files, err := proxyFileListToHost(context.Background(), hostAddr, "runner-1", "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed []map[string]any
	json.Unmarshal(files, &parsed)
	if len(parsed) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(parsed))
	}
}

// --- Helpers ---

// createTestMCPServer builds an MCP server with all tools registered,
// using the same factory as newMCPHandler but without HTTP/auth wrapping.
func createTestMCPServer(deps *mcpDeps) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "test-server", Version: "0.0.1"},
		nil,
	)
	mcp.AddTool(server, &mcp.Tool{Name: "allocate_sandbox", Description: "test"}, deps.handleAllocate)
	mcp.AddTool(server, &mcp.Tool{Name: "release_sandbox", Description: "test"}, deps.handleRelease)
	mcp.AddTool(server, &mcp.Tool{Name: "pause_sandbox", Description: "test"}, deps.handlePause)
	mcp.AddTool(server, &mcp.Tool{Name: "resume_sandbox", Description: "test"}, deps.handleResume)
	mcp.AddTool(server, &mcp.Tool{Name: "get_sandbox_status", Description: "test"}, deps.handleGetStatus)
	mcp.AddTool(server, &mcp.Tool{Name: "list_sandboxes", Description: "test"}, deps.handleListSandboxes)
	mcp.AddTool(server, &mcp.Tool{Name: "list_hosts", Description: "test"}, deps.handleListHosts)
	mcp.AddTool(server, &mcp.Tool{Name: "exec_command", Description: "test"}, deps.handleExec)
	mcp.AddTool(server, &mcp.Tool{Name: "read_file", Description: "test"}, deps.handleReadFile)
	mcp.AddTool(server, &mcp.Tool{Name: "write_file", Description: "test"}, deps.handleWriteFile)
	mcp.AddTool(server, &mcp.Tool{Name: "list_files", Description: "test"}, deps.handleListFiles)
	return server
}

func connectTestClient(t *testing.T, ctx context.Context, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	return session
}
