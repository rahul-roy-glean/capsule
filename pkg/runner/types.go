package runner

import (
	"net"
	"sync"
	"time"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/authproxy"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// State represents the state of a runner
type State string

const (
	StateCold         State = "cold"
	StateBooting      State = "booting"
	StateInitializing State = "initializing"
	StateIdle         State = "idle"
	StateBusy         State = "busy"
	StateDraining     State = "draining"
	StateQuarantined  State = "quarantined"
	StateRetiring     State = "retiring"
	StateTerminated   State = "terminated"
	StatePaused       State = "paused" // VM paused and eligible for pool reuse
	StatePausing      State = "pausing"
	StateSuspended    State = "suspended"
)

// Runner represents a single runner instance
type Runner struct {
	mu                      sync.Mutex `json:"-"`
	ID                      string
	HostID                  string
	State                   State
	InternalIP              net.IP
	TapDevice               string
	MAC                     string
	SnapshotVersion         string
	WorkloadKey             string // Workload key for snapshot routing in multi-snapshot support
	JobID                   string
	Resources               Resources
	CreatedAt               time.Time
	StartedAt               time.Time
	CompletedAt             time.Time
	LastHeartbeat           time.Time
	SocketPath              string
	LogPath                 string
	MetricsPath             string
	RootfsOverlay           string
	QuarantineReason        string
	QuarantinedAt           time.Time
	QuarantineDir           string
	PreQuarantineState      State
	QuarantineEgressBlocked bool
	QuarantinePaused        bool
	ServicePort int                   // Port of the user's service inside the VM (from StartCommand)
	AuthConfig  *authproxy.AuthConfig // Auth proxy config, preserved for pause/resume

	// Network policy fields
	NetworkPolicy          *network.NetworkPolicy `json:"network_policy,omitempty"`
	NetworkPolicyVersion   int                    `json:"network_policy_version,omitempty"`
	PreQuarantinePolicy    *network.NetworkPolicy `json:"pre_quarantine_policy,omitempty"`
	DynamicDomainsAdded    int                    `json:"dynamic_domains_added,omitempty"`
	DynamicCIDRsAdded      int                    `json:"dynamic_cidrs_added,omitempty"`
	EmergencyEgressBlocked bool                   `json:"emergency_egress_blocked,omitempty"`

	PausedAt time.Time `json:"paused_at,omitempty"`

	// Session pause/resume fields
	SessionID     string    `json:"session_id,omitempty"`
	TTLSeconds    int       `json:"ttl_seconds,omitempty"`
	AutoPause     bool      `json:"auto_pause,omitempty"`
	LastExecAt    time.Time `json:"last_exec_at,omitempty"`
	ActiveExecs   int32     `json:"active_execs,omitempty"`
	SessionDir    string    `json:"session_dir,omitempty"`
	SessionLayers int       `json:"session_layers,omitempty"`
}

// RunnerHeartbeatInfo is a lightweight per-runner status included in host
// heartbeats so the control plane can enforce TTLs centrally.
type RunnerHeartbeatInfo struct {
	RunnerID    string `json:"runner_id"`
	State       State  `json:"state"`
	WorkloadKey string `json:"workload_key"`
	IdleSince   string `json:"idle_since,omitempty"` // RFC3339; set when idle and LastExecAt is non-zero
}

// Resources represents the resources allocated to a runner
type Resources struct {
	VCPUs    int
	MemoryMB int
	DiskGB   int
}

// AllocateRequest represents a request to allocate a runner
type AllocateRequest struct {
	RequestID           string
	WorkloadKey         string // Workload key identifying which snapshot to use for this runner
	SnapshotVersion     string // Explicit snapshot version; skips current-pointer.json lookup when set
	Resources           Resources
	Labels              map[string]string
	StartCommand        *snapshot.StartCommand // Optional: user service to start inside the VM
	SessionID           string                 // optional: bind to session for pause/resume
	TTLSeconds          int                    // idle timeout from snapshot config
	AutoPause           bool                   // pause on TTL vs destroy
	SnapshotTag         string                 // optional: named tag to resolve snapshot version
	NetworkPolicyPreset string                 // optional: named preset (e.g., "restricted-egress")
	NetworkPolicy       *network.NetworkPolicy // optional: full policy override
	AuthConfig          *authproxy.AuthConfig  // optional: auth proxy configuration
}

// MMDSData represents data to inject into the microVM via MMDS
type MMDSData struct {
	Latest struct {
		Meta struct {
			RunnerID     string            `json:"runner_id"`
			HostID       string            `json:"host_id"`
			InstanceName string            `json:"instance_name,omitempty"`
			Environment  string            `json:"environment"`
			JobID        string            `json:"job_id,omitempty"`
			Mode         string            `json:"mode,omitempty"`         // "warmup", "exec", or empty for normal runner
			CurrentTime  string            `json:"current_time,omitempty"` // RFC3339 timestamp from host for clock sync
			Labels       map[string]string `json:"labels,omitempty"`
		} `json:"meta"`
		Network struct {
			IP        string `json:"ip"`
			Gateway   string `json:"gateway"`
			Netmask   string `json:"netmask"`
			DNS       string `json:"dns"`
			Interface string `json:"interface"`
			MAC       string `json:"mac"`
		} `json:"network"`
		Snapshot struct {
			Version string `json:"version"`
		} `json:"snapshot"`
		Exec struct {
			Command    []string          `json:"command,omitempty"`
			Env        map[string]string `json:"env,omitempty"`
			WorkingDir string            `json:"working_dir,omitempty"`
			TimeoutSec int               `json:"timeout_seconds,omitempty"`
		} `json:"exec,omitempty"`
		// Mirrors snapshot.StartCommand — keep in sync with pkg/snapshot/start_command.go.
		StartCommand struct {
			Command    []string          `json:"command,omitempty"`
			Port       int               `json:"port,omitempty"`
			HealthPath string            `json:"health_path,omitempty"`
			Env        map[string]string `json:"env,omitempty"`
			RunAs      string            `json:"run_as,omitempty"`
		} `json:"start_command,omitempty"`
		// Drives lists extension drives to mount inside the VM.
		// Populated from DriveSpec metadata in the snapshot.
		Drives []snapshot.DriveSpec `json:"drives,omitempty"`
		Warmup struct {
			Commands []snapshot.SnapshotCommand `json:"commands,omitempty"`
		} `json:"warmup,omitempty"`
		Proxy struct {
			CACertPEM    string `json:"ca_cert_pem,omitempty"`
			Address      string `json:"address,omitempty"`
			MetadataHost string `json:"metadata_host,omitempty"`
		} `json:"proxy,omitempty"`
	} `json:"latest"`
}

// HostConfig holds configuration for the host agent.
// Core fields handle generic VM lifecycle.
type HostConfig struct {
	HostID            string
	InstanceName      string
	Zone              string
	MaxRunners        int
	IdleTarget        int
	FirecrackerBin    string
	SocketDir         string
	WorkspaceDir      string
	LogDir            string
	SnapshotBucket    string
	SnapshotCachePath string

	QuarantineDir string
	// SessionDir is the base directory for session snapshot storage (pause/resume).
	// Defaults to {SnapshotCachePath}/../sessions (e.g. /mnt/data/sessions).
	SessionDir string
	// WorkloadKey identifies which snapshot this host should use (hash of snapshot commands).
	WorkloadKey      string
	Environment      string
	ControlPlaneAddr string
	GCPProject       string

	// Host resource capacity for bin-packing scheduler
	TotalCPUMillicores int
	TotalMemoryMB      int

	// SessionChunkBucket enables cloud-backed session pause/resume.
	SessionChunkBucket string
	// GCSPrefix is the top-level prefix for all GCS paths (e.g. "v1").
	GCSPrefix string
}
