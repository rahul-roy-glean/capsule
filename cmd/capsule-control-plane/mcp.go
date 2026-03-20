package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/rahul-roy-glean/capsule/api/proto/runner"
	fcmcp "github.com/rahul-roy-glean/capsule/pkg/mcp"
)

// mcpDeps holds control plane dependencies needed by MCP tool handlers.
type mcpDeps struct {
	scheduler    *Scheduler
	hostRegistry *HostRegistry
	db           *sql.DB
	logger       *logrus.Entry
}

// --- Input/Output structs for MCP tools ---

type AllocateSandboxInput struct {
	WorkloadKey         string `json:"workload_key" jsonschema:"Snapshot workload key identifying which VM image to boot"`
	SessionID           string `json:"session_id,omitempty" jsonschema:"Session ID for pause/resume. If a matching suspended session exists it resumes."`
	SnapshotTag         string `json:"snapshot_tag,omitempty" jsonschema:"Named snapshot tag (e.g. stable or canary)"`
	NetworkPolicyPreset string `json:"network_policy_preset,omitempty" jsonschema:"Named network policy preset (e.g. restricted-egress or isolated)"`
}

type AllocateSandboxOutput struct {
	SandboxID   string `json:"sandbox_id"`
	HostID      string `json:"host_id"`
	HostAddress string `json:"host_address"`
	InternalIP  string `json:"internal_ip"`
	SessionID   string `json:"session_id"`
	Resumed     bool   `json:"resumed"`
}

type ReleaseSandboxInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"ID of the sandbox to release"`
}

type ReleaseSandboxOutput struct {
	Success bool `json:"success"`
}

type PauseSandboxInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"ID of the sandbox to pause"`
}

type PauseSandboxOutput struct {
	Success           bool   `json:"success"`
	SessionID         string `json:"session_id"`
	SnapshotSizeBytes int64  `json:"snapshot_size_bytes"`
	Layer             int32  `json:"layer"`
}

type ResumeSandboxInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"ID of the sandbox (runner_id) to resume"`
}

type ResumeSandboxOutput struct {
	Status      string `json:"status"`
	SandboxID   string `json:"sandbox_id"`
	HostAddress string `json:"host_address"`
}

type GetSandboxStatusInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"ID of the sandbox to query"`
}

type GetSandboxStatusOutput struct {
	SandboxID   string `json:"sandbox_id"`
	Status      string `json:"status"`
	HostID      string `json:"host_id,omitempty"`
	HostAddress string `json:"host_address,omitempty"`
	InternalIP  string `json:"internal_ip,omitempty"`
	WorkloadKey string `json:"workload_key,omitempty"`
}

type ListSandboxesInput struct {
	WorkloadKey string `json:"workload_key,omitempty" jsonschema:"Filter by workload key"`
	Status      string `json:"status,omitempty" jsonschema:"Filter by status (e.g. running or suspended)"`
}

type SandboxInfo struct {
	SandboxID   string `json:"sandbox_id"`
	HostID      string `json:"host_id"`
	HostName    string `json:"host_name"`
	WorkloadKey string `json:"workload_key"`
	Status      string `json:"status"`
}

type ListSandboxesOutput struct {
	Sandboxes []SandboxInfo `json:"sandboxes"`
	Count     int           `json:"count"`
}

type ListHostsInput struct{}

type HostInfo struct {
	ID              string    `json:"id"`
	InstanceName    string    `json:"instance_name"`
	Zone            string    `json:"zone"`
	Status          string    `json:"status"`
	IdleRunners     int       `json:"idle_runners"`
	BusyRunners     int       `json:"busy_runners"`
	TotalCPU        int       `json:"total_cpu_millicores"`
	UsedCPU         int       `json:"used_cpu_millicores"`
	TotalMemoryMB   int       `json:"total_memory_mb"`
	UsedMemoryMB    int       `json:"used_memory_mb"`
	LastHeartbeat   time.Time `json:"last_heartbeat"`
	SnapshotVersion string    `json:"snapshot_version,omitempty"`
}

type ListHostsOutput struct {
	Hosts []HostInfo `json:"hosts"`
	Count int        `json:"count"`
}

type ExecCommandInput struct {
	SandboxID   string            `json:"sandbox_id" jsonschema:"ID of the sandbox to execute in"`
	Command     string            `json:"command" jsonschema:"Shell command to execute"`
	WorkingDir  string            `json:"working_dir,omitempty" jsonschema:"Working directory for the command"`
	Env         map[string]string `json:"env,omitempty" jsonschema:"Additional environment variables"`
	TimeoutSecs int               `json:"timeout_secs,omitempty" jsonschema:"Timeout in seconds (default 300)"`
}

type ExecCommandOutput struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type ReadFileInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"ID of the sandbox to read from"`
	Path      string `json:"path" jsonschema:"Absolute file path inside the sandbox"`
	Offset    int    `json:"offset,omitempty" jsonschema:"Byte offset to start reading from"`
	Limit     int    `json:"limit,omitempty" jsonschema:"Maximum bytes to read"`
}

type ReadFileOutput struct {
	Content string `json:"content"`
}

type WriteFileInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"ID of the sandbox to write to"`
	Path      string `json:"path" jsonschema:"Absolute file path inside the sandbox"`
	Content   string `json:"content" jsonschema:"File content to write"`
	Mode      string `json:"mode,omitempty" jsonschema:"File mode (e.g. 0644). Default is 0644."`
}

type WriteFileOutput struct {
	Success bool `json:"success"`
}

type ListFilesInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"ID of the sandbox"`
	Path      string `json:"path" jsonschema:"Directory path to list"`
}

type ListFilesOutput struct {
	Files json.RawMessage `json:"files"`
}

// --- Tool handlers ---

func (m *mcpDeps) handleAllocate(ctx context.Context, _ *mcp.CallToolRequest, in AllocateSandboxInput) (*mcp.CallToolResult, AllocateSandboxOutput, error) {
	resp, err := m.scheduler.AllocateRunner(ctx, AllocateRunnerRequest{
		RequestID:           fmt.Sprintf("mcp-%d", time.Now().UnixNano()),
		WorkloadKey:         in.WorkloadKey,
		Source:              "mcp",
		SessionID:           in.SessionID,
		SnapshotTag:         in.SnapshotTag,
		NetworkPolicyPreset: in.NetworkPolicyPreset,
	})
	if err != nil {
		return nil, AllocateSandboxOutput{}, fmt.Errorf("allocation failed: %w", err)
	}
	return nil, AllocateSandboxOutput{
		SandboxID:   resp.RunnerID,
		HostID:      resp.HostID,
		HostAddress: resp.HostAddress,
		InternalIP:  resp.InternalIP,
		SessionID:   resp.SessionID,
		Resumed:     resp.Resumed,
	}, nil
}

func (m *mcpDeps) handleRelease(ctx context.Context, _ *mcp.CallToolRequest, in ReleaseSandboxInput) (*mcp.CallToolResult, ReleaseSandboxOutput, error) {
	if err := m.scheduler.ReleaseRunner(ctx, in.SandboxID, true); err != nil {
		return nil, ReleaseSandboxOutput{}, fmt.Errorf("release failed: %w", err)
	}
	return nil, ReleaseSandboxOutput{Success: true}, nil
}

func (m *mcpDeps) handlePause(ctx context.Context, _ *mcp.CallToolRequest, in PauseSandboxInput) (*mcp.CallToolResult, PauseSandboxOutput, error) {
	runner, err := m.hostRegistry.GetRunner(in.SandboxID)
	if err != nil {
		return nil, PauseSandboxOutput{}, fmt.Errorf("runner not found: %w", err)
	}
	host, err := m.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		return nil, PauseSandboxOutput{}, fmt.Errorf("host not found: %w", err)
	}

	conn, err := grpc.NewClient(host.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, PauseSandboxOutput{}, fmt.Errorf("connect to host: %w", err)
	}
	defer conn.Close()

	client := pb.NewHostAgentClient(conn)
	resp, err := client.PauseRunner(ctx, &pb.PauseRunnerRequest{RunnerId: in.SandboxID})
	if err != nil {
		return nil, PauseSandboxOutput{}, fmt.Errorf("pause failed: %w", err)
	}
	if resp.Error != "" {
		return nil, PauseSandboxOutput{}, fmt.Errorf("pause error: %s", resp.Error)
	}

	// Update session_snapshots table
	if resp.SessionId != "" && m.db != nil {
		var networkPolicy any
		if runner.NetworkPolicyJSON != "" {
			networkPolicy = runner.NetworkPolicyJSON
		}
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO session_snapshots (
				session_id, runner_id, workload_key, host_id, status, layer_count, paused_at,
				runner_ttl_seconds, auto_pause, network_policy_preset, network_policy
			)
			VALUES ($1, $2, $3, $4, 'suspended', $5, NOW(), $6, $7, $8, $9)
			ON CONFLICT (session_id) DO UPDATE SET
				status = 'suspended',
				layer_count = EXCLUDED.layer_count,
				paused_at = NOW(),
				runner_ttl_seconds = EXCLUDED.runner_ttl_seconds,
				auto_pause = EXCLUDED.auto_pause,
				network_policy_preset = EXCLUDED.network_policy_preset,
				network_policy = EXCLUDED.network_policy
		`, resp.SessionId, in.SandboxID, runner.WorkloadKey, host.ID, resp.Layer+1,
			runner.RunnerTTLSeconds, runner.AutoPause, runner.NetworkPolicyPreset, networkPolicy)
	}

	_ = m.hostRegistry.RemoveRunner(in.SandboxID)

	return nil, PauseSandboxOutput{
		Success:           resp.Success,
		SessionID:         resp.SessionId,
		SnapshotSizeBytes: resp.SnapshotSizeBytes,
		Layer:             resp.Layer,
	}, nil
}

func (m *mcpDeps) handleResume(ctx context.Context, _ *mcp.CallToolRequest, in ResumeSandboxInput) (*mcp.CallToolResult, ResumeSandboxOutput, error) {
	// Check if runner is already live
	if _, err := m.hostRegistry.GetRunner(in.SandboxID); err == nil {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("sandbox %s is already running, use exec_command directly", in.SandboxID)
	}

	// Look up suspended session
	var sessionID, hostID, workloadKey, status string
	var sessionTTL sql.NullInt64
	var sessionAutoPause sql.NullBool
	var sessionNPPreset sql.NullString
	var sessionNPJSON sql.NullString
	if m.db == nil {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("no database configured")
	}
	err := m.db.QueryRowContext(ctx,
		`SELECT session_id, host_id, workload_key, status, runner_ttl_seconds, auto_pause, network_policy_preset, network_policy
		 FROM session_snapshots WHERE runner_id = $1`,
		in.SandboxID).Scan(&sessionID, &hostID, &workloadKey, &status, &sessionTTL, &sessionAutoPause, &sessionNPPreset, &sessionNPJSON)
	if err != nil || status != "suspended" {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("no suspended session found for sandbox %s", in.SandboxID)
	}

	// Pick host to resume on
	var resumeHost *Host
	origHost, origErr := m.hostRegistry.GetHost(hostID)
	if origErr == nil && origHost.Status != "draining" && origHost.Status != "terminating" {
		resumeHost = origHost
	} else {
		resumeHost = m.scheduler.selectBestHostForWorkloadKey(m.hostRegistry.GetAvailableHosts(), workloadKey)
	}
	if resumeHost == nil {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("no available host for session resume")
	}

	conn, err := grpc.NewClient(resumeHost.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("connect to host: %w", err)
	}
	defer conn.Close()

	client := pb.NewHostAgentClient(conn)
	resumeReq := &pb.ResumeRunnerRequest{
		SessionId:           sessionID,
		WorkloadKey:         workloadKey,
		TtlSeconds:          int32(sessionTTL.Int64),
		AutoPause:           sessionAutoPause.Valid && sessionAutoPause.Bool,
		NetworkPolicyPreset: sessionNPPreset.String,
	}
	if sessionNPJSON.Valid {
		resumeReq.NetworkPolicyJson = sessionNPJSON.String
	}
	resp, err := client.ResumeRunner(ctx, resumeReq)
	if err != nil {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("resume failed: %w", err)
	}
	if resp.Error != "" {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("resume error: %s", resp.Error)
	}

	resumedRunnerID := resp.Runner.GetId()
	if resumedRunnerID == "" {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("resume succeeded but returned empty runner id")
	}

	if err := m.hostRegistry.AddRunner(ctx, &Runner{
		ID:                  resumedRunnerID,
		HostID:              resumeHost.ID,
		Status:              "busy",
		InternalIP:          resp.Runner.GetInternalIp(),
		SessionID:           sessionID,
		WorkloadKey:         workloadKey,
		RunnerTTLSeconds:    int(sessionTTL.Int64),
		AutoPause:           sessionAutoPause.Valid && sessionAutoPause.Bool,
		NetworkPolicyPreset: sessionNPPreset.String,
		NetworkPolicyJSON:   sessionNPJSON.String,
	}); err != nil {
		return nil, ResumeSandboxOutput{}, fmt.Errorf("register resumed runner: %w", err)
	}

	// Update session status
	_, _ = m.db.ExecContext(ctx,
		`UPDATE session_snapshots SET runner_id = $1, status = 'active', host_id = $2 WHERE session_id = $3`,
		resumedRunnerID, resumeHost.ID, sessionID)

	return nil, ResumeSandboxOutput{
		Status:      "resumed",
		SandboxID:   resumedRunnerID,
		HostAddress: resumeHost.HTTPAddress,
	}, nil
}

func (m *mcpDeps) handleGetStatus(ctx context.Context, _ *mcp.CallToolRequest, in GetSandboxStatusInput) (*mcp.CallToolResult, GetSandboxStatusOutput, error) {
	runner, err := m.hostRegistry.GetRunner(in.SandboxID)
	if err == nil {
		host, _ := m.hostRegistry.GetHost(runner.HostID)
		out := GetSandboxStatusOutput{
			SandboxID:   in.SandboxID,
			Status:      runner.Status,
			HostID:      runner.HostID,
			InternalIP:  runner.InternalIP,
			WorkloadKey: runner.WorkloadKey,
		}
		if host != nil {
			out.HostAddress = host.HTTPAddress
		}
		return nil, out, nil
	}

	// Fallback: check session_snapshots
	if m.db != nil {
		var status string
		scanErr := m.db.QueryRowContext(ctx,
			`SELECT status FROM session_snapshots WHERE runner_id = $1`, in.SandboxID).Scan(&status)
		if scanErr == nil {
			return nil, GetSandboxStatusOutput{
				SandboxID: in.SandboxID,
				Status:    status,
			}, nil
		}
	}

	return nil, GetSandboxStatusOutput{}, fmt.Errorf("sandbox not found: %s", in.SandboxID)
}

func (m *mcpDeps) handleListSandboxes(_ context.Context, _ *mcp.CallToolRequest, in ListSandboxesInput) (*mcp.CallToolResult, ListSandboxesOutput, error) {
	hosts := m.hostRegistry.GetAllHosts()
	var sandboxes []SandboxInfo

	for _, h := range hosts {
		m.hostRegistry.mu.RLock()
		runnerInfos := h.RunnerInfos
		m.hostRegistry.mu.RUnlock()

		for _, ri := range runnerInfos {
			if in.WorkloadKey != "" && ri.WorkloadKey != in.WorkloadKey {
				continue
			}
			if in.Status != "" && ri.State != in.Status {
				continue
			}
			sandboxes = append(sandboxes, SandboxInfo{
				SandboxID:   ri.RunnerID,
				HostID:      h.ID,
				HostName:    h.InstanceName,
				WorkloadKey: ri.WorkloadKey,
				Status:      ri.State,
			})
		}
	}

	return nil, ListSandboxesOutput{
		Sandboxes: sandboxes,
		Count:     len(sandboxes),
	}, nil
}

func (m *mcpDeps) handleListHosts(_ context.Context, _ *mcp.CallToolRequest, _ ListHostsInput) (*mcp.CallToolResult, ListHostsOutput, error) {
	hosts := m.hostRegistry.GetAllHosts()
	out := ListHostsOutput{
		Hosts: make([]HostInfo, 0, len(hosts)),
		Count: len(hosts),
	}
	for _, h := range hosts {
		out.Hosts = append(out.Hosts, HostInfo{
			ID:              h.ID,
			InstanceName:    h.InstanceName,
			Zone:            h.Zone,
			Status:          h.Status,
			IdleRunners:     h.IdleRunners,
			BusyRunners:     h.BusyRunners,
			TotalCPU:        h.TotalCPUMillicores,
			UsedCPU:         h.UsedCPUMillicores,
			TotalMemoryMB:   h.TotalMemoryMB,
			UsedMemoryMB:    h.UsedMemoryMB,
			LastHeartbeat:   h.LastHeartbeat,
			SnapshotVersion: h.SnapshotVersion,
		})
	}
	return nil, out, nil
}

func (m *mcpDeps) handleExec(ctx context.Context, _ *mcp.CallToolRequest, in ExecCommandInput) (*mcp.CallToolResult, ExecCommandOutput, error) {
	hostHTTP, err := m.resolveHostHTTP(in.SandboxID)
	if err != nil {
		return nil, ExecCommandOutput{}, err
	}

	result, err := proxyExecToHost(ctx, hostHTTP, in.SandboxID, in.Command, in.WorkingDir, in.Env, in.TimeoutSecs)
	if err != nil {
		return nil, ExecCommandOutput{}, fmt.Errorf("exec failed: %w", err)
	}

	return nil, ExecCommandOutput{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	}, nil
}

func (m *mcpDeps) handleReadFile(ctx context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
	hostHTTP, err := m.resolveHostHTTP(in.SandboxID)
	if err != nil {
		return nil, ReadFileOutput{}, err
	}

	content, err := proxyFileReadToHost(ctx, hostHTTP, in.SandboxID, in.Path, in.Offset, in.Limit)
	if err != nil {
		return nil, ReadFileOutput{}, fmt.Errorf("read_file failed: %w", err)
	}

	return nil, ReadFileOutput{Content: content}, nil
}

func (m *mcpDeps) handleWriteFile(ctx context.Context, _ *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
	hostHTTP, err := m.resolveHostHTTP(in.SandboxID)
	if err != nil {
		return nil, WriteFileOutput{}, err
	}

	if err := proxyFileWriteToHost(ctx, hostHTTP, in.SandboxID, in.Path, in.Content, in.Mode); err != nil {
		return nil, WriteFileOutput{}, fmt.Errorf("write_file failed: %w", err)
	}

	return nil, WriteFileOutput{Success: true}, nil
}

func (m *mcpDeps) handleListFiles(ctx context.Context, _ *mcp.CallToolRequest, in ListFilesInput) (*mcp.CallToolResult, ListFilesOutput, error) {
	hostHTTP, err := m.resolveHostHTTP(in.SandboxID)
	if err != nil {
		return nil, ListFilesOutput{}, err
	}

	files, err := proxyFileListToHost(ctx, hostHTTP, in.SandboxID, in.Path)
	if err != nil {
		return nil, ListFilesOutput{}, fmt.Errorf("list_files failed: %w", err)
	}

	return nil, ListFilesOutput{Files: files}, nil
}

// resolveHostHTTP looks up the host HTTP address for a given sandbox/runner ID.
func (m *mcpDeps) resolveHostHTTP(sandboxID string) (string, error) {
	runner, err := m.hostRegistry.GetRunner(sandboxID)
	if err != nil {
		return "", fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	host, err := m.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		return "", fmt.Errorf("host not found for sandbox: %s", sandboxID)
	}
	hostHTTP, err := hostAgentHTTPAddress(host.GRPCAddress)
	if err != nil {
		return "", fmt.Errorf("resolve host HTTP address: %w", err)
	}
	return hostHTTP, nil
}

// --- Server factory ---

// newMCPHandler creates an http.Handler serving the MCP protocol with all
// sandbox lifecycle and in-VM operation tools. If authToken is non-empty,
// requests are authenticated via Bearer token.
func newMCPHandler(deps *mcpDeps, authToken string) http.Handler {
	createServer := func(_ *http.Request) *mcp.Server {
		server := mcp.NewServer(
			&mcp.Implementation{
				Name:    "capsule",
				Version: "1.0.0",
			},
			&mcp.ServerOptions{
				Instructions: "Sandbox orchestration for capsule. " +
					"Use allocate_sandbox to create VMs, exec_command to run commands, " +
					"and pause_sandbox/resume_sandbox for session management.",
			},
		)

		// Lifecycle tools
		mcp.AddTool(server, &mcp.Tool{
			Name:        "allocate_sandbox",
			Description: "Allocate a new sandbox VM from a snapshot workload key. Returns sandbox_id for subsequent operations.",
		}, deps.handleAllocate)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "release_sandbox",
			Description: "Release and destroy a sandbox VM. The sandbox is permanently terminated.",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptrTrue},
		}, deps.handleRelease)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "pause_sandbox",
			Description: "Pause a running sandbox, creating an incremental memory snapshot. The sandbox can be resumed later with resume_sandbox.",
		}, deps.handlePause)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "resume_sandbox",
			Description: "Resume a previously paused sandbox from its snapshot. Pass the original sandbox_id (runner_id).",
		}, deps.handleResume)

		// Status/discovery tools
		mcp.AddTool(server, &mcp.Tool{
			Name:        "get_sandbox_status",
			Description: "Get the current status of a sandbox (running, suspended, etc.) and its host address.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, deps.handleGetStatus)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list_sandboxes",
			Description: "List all active sandboxes across the fleet. Optionally filter by workload_key or status.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, deps.handleListSandboxes)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list_hosts",
			Description: "List all host machines in the fleet with resource utilization and status.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, deps.handleListHosts)

		// In-VM operation tools
		mcp.AddTool(server, &mcp.Tool{
			Name:        "exec_command",
			Description: "Execute a shell command inside a sandbox and return stdout, stderr, and exit code. Commands run synchronously with a configurable timeout.",
		}, deps.handleExec)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_file",
			Description: "Read the contents of a file inside a sandbox.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, deps.handleReadFile)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "write_file",
			Description: "Write content to a file inside a sandbox. Creates the file if it doesn't exist.",
		}, deps.handleWriteFile)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list_files",
			Description: "List files and directories at a given path inside a sandbox.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, deps.handleListFiles)

		return server
	}

	handler := mcp.NewStreamableHTTPHandler(createServer, &mcp.StreamableHTTPOptions{
		Stateless: true,
	})

	if authToken != "" {
		middleware := auth.RequireBearerToken(fcmcp.StaticTokenVerifier(authToken), nil)
		return middleware(handler)
	}

	return handler
}

var ptrTrue = func() *bool { b := true; return &b }()
