package runner

import (
	"net"
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
	ID                      string
	HostID                  string
	State                   State
	InternalIP              net.IP
	TapDevice               string
	MAC                     string
	SnapshotVersion         string
	GitHubRunnerID          string
	GitHubRepo              string // Repository for pool key matching
	WorkloadKey             string // Workload key for snapshot routing in multi-snapshot support
	CISystem                string // CI system used for this runner (e.g., "github-actions")
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
	RepoCacheUpper          string
	QuarantineReason        string
	QuarantinedAt           time.Time
	QuarantineDir           string
	PreQuarantineState      State
	QuarantineEgressBlocked bool
	QuarantinePaused        bool
	ServicePort             int // Port of the user's service inside the VM (from StartCommand)

	// Network policy fields
	NetworkPolicy          *network.NetworkPolicy `json:"network_policy,omitempty"`
	NetworkPolicyVersion   int                    `json:"network_policy_version,omitempty"`
	PreQuarantinePolicy    *network.NetworkPolicy `json:"pre_quarantine_policy,omitempty"`
	DynamicDomainsAdded    int                    `json:"dynamic_domains_added,omitempty"`
	DynamicCIDRsAdded      int                    `json:"dynamic_cidrs_added,omitempty"`
	EmergencyEgressBlocked bool                   `json:"emergency_egress_blocked,omitempty"`

	// Pool-related fields
	PoolKey          *RunnerKey `json:"pool_key,omitempty"`
	PausedAt         time.Time  `json:"paused_at,omitempty"`
	TaskCount        int        `json:"task_count"`
	MemoryUsageBytes int64      `json:"memory_usage_bytes,omitempty"`
	DiskUsageBytes   int64      `json:"disk_usage_bytes,omitempty"`

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
	Repo                string
	Branch              string
	Commit              string
	WorkloadKey         string // Workload key identifying which snapshot to use for this runner
	SnapshotVersion     string // Explicit snapshot version; skips current-pointer.json lookup when set
	Resources           Resources
	Labels              map[string]string
	GitHubRunnerToken   string
	CISystem            string                 // CI system identifier
	StartCommand        *snapshot.StartCommand // Optional: user service to start inside the VM
	SessionID           string                 // optional: bind to session for pause/resume
	TTLSeconds          int                    // idle timeout from snapshot config
	AutoPause           bool                   // pause on TTL vs destroy
	SnapshotTag         string                 // optional: named tag to resolve snapshot version
	NetworkPolicyPreset string                 // optional: named preset (e.g., "ci-standard")
	NetworkPolicy       *network.NetworkPolicy // optional: full policy override
	AuthConfig          *authproxy.AuthConfig  // optional: auth proxy configuration
}

// MMDSData represents data to inject into the microVM via MMDS
type MMDSData struct {
	Latest struct {
		Meta struct {
			RunnerID     string `json:"runner_id"`
			HostID       string `json:"host_id"`
			InstanceName string `json:"instance_name,omitempty"`
			Environment  string `json:"environment"`
			JobID        string `json:"job_id,omitempty"`
			Mode         string `json:"mode,omitempty"`         // "warmup", "exec", or empty for normal runner
			CurrentTime  string `json:"current_time,omitempty"` // RFC3339 timestamp from host for clock sync
		} `json:"meta"`
		Buildbarn struct {
			// CertsMountPath is where Buildbarn mTLS certs will be mounted inside the microVM.
			// Some existing setups use /etc/glean/ci/certs; this is configurable.
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
			GitToken          string            `json:"git_token"` // Installation token for git clone auth (private repos)
			Labels            map[string]string `json:"labels"`
		} `json:"job"`
		Runner struct {
			Ephemeral bool   `json:"ephemeral"`
			CISystem  string `json:"ci_system,omitempty"`
		} `json:"runner,omitempty"`
		Snapshot struct {
			Version string `json:"version"`
		} `json:"snapshot"`
		GitCache struct {
			// Enabled indicates whether git-cache reference cloning is available
			Enabled bool `json:"enabled"`
			// MountPath is where the git-cache block device is mounted inside the microVM
			MountPath string `json:"mount_path,omitempty"`
			// RepoMappings maps repo URLs/names to their cache paths inside MountPath
			// e.g. {"github.com/org/repo": "org-repo"} means /mnt/git-cache/org-repo
			RepoMappings map[string]string `json:"repo_mappings,omitempty"`
			// WorkspaceDir is the target directory for cloned repositories
			WorkspaceDir string `json:"workspace_dir,omitempty"`
			// PreClonedPath is the path where the repo was pre-cloned during warmup
			// (baked into the snapshot rootfs). Thaw-agent creates a symlink from WorkspaceDir to here.
			PreClonedPath string `json:"pre_cloned_path,omitempty"`
		} `json:"git_cache,omitempty"`
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
	} `json:"latest"`
}

// CredentialSecret defines a secret to fetch from GCP Secret Manager.
type CredentialSecret struct {
	Name       string `json:"name"`        // Human-readable name
	SecretName string `json:"secret_name"` // Full Secret Manager path
	Target     string `json:"target"`      // Target path inside credentials drive
}

// CredentialHostDir defines a host directory to include in the credentials drive.
type CredentialHostDir struct {
	Name     string `json:"name"`      // Human-readable name
	HostPath string `json:"host_path"` // Path on the host
	Target   string `json:"target"`    // Target path inside credentials drive
}

// CIConfig holds optional CI system integration settings.
// When CISystem is empty or "none", the platform operates as a generic VM host.
type CIConfig struct {
	CISystem              string
	GitHubRunnerEnabled   bool
	GitHubRepo            string
	GitHubOrg             string
	GitHubRunnerLabels    []string
	GitHubRunnerEphemeral bool
	GitHubAppID           string
	GitHubAppSecret       string
}

// BazelConfig holds optional Bazel-specific settings for CI workloads.
type BazelConfig struct {
	RepoCacheUpperSizeGB      int
	BuildbarnCertsDir         string
	BuildbarnCertsMountPath   string
	BuildbarnCertsImageSizeMB int
	GitCacheEnabled           bool
	GitCacheDir               string
	GitCacheImagePath         string
	GitCacheMountPath         string
	GitCacheRepoMappings      map[string]string
	GitCacheWorkspaceDir      string
	GitCachePreClonedPath     string
}

// HostConfig holds configuration for the host agent.
// Core fields handle generic VM lifecycle. CI and Bazel-specific settings
// are isolated in the CI and Bazel sub-structs respectively.
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

	// Credentials configuration
	CredentialsSecrets     []CredentialSecret
	CredentialsHostDirs    []CredentialHostDir
	CredentialsEnv         map[string]string
	CredentialsImageSizeMB int

	QuarantineDir string
	// SessionDir is the base directory for session snapshot storage (pause/resume).
	// Defaults to {SnapshotCachePath}/../sessions (e.g. /mnt/data/sessions).
	SessionDir        string
	MicroVMSubnet     string
	ExternalInterface string
	BridgeName        string
	// WorkloadKey identifies which snapshot this host should use (hash of snapshot commands).
	WorkloadKey      string
	Environment      string
	ControlPlaneAddr string
	GCPProject       string

	// Host resource capacity for bin-packing scheduler
	TotalCPUMillicores int
	TotalMemoryMB      int

	// Runner Pool Configuration
	PoolEnabled            bool `json:"pool_enabled"`
	PoolMaxRunners         int  `json:"pool_max_runners"`
	PoolMaxTotalMemoryGB   int  `json:"pool_max_total_memory_gb"`
	PoolMaxRunnerMemoryGB  int  `json:"pool_max_runner_memory_gb"`
	PoolMaxRunnerDiskGB    int  `json:"pool_max_runner_disk_gb"`
	PoolRecycleTimeoutSecs int  `json:"pool_recycle_timeout_secs"`

	// SessionChunkBucket enables cloud-backed session pause/resume.
	SessionChunkBucket string
	// GCSPrefix is the top-level prefix for all GCS paths (e.g. "v1").
	GCSPrefix string

	// CI holds optional CI system integration settings.
	// When CI.CISystem is empty or "none", the platform operates as a generic VM host.
	CI CIConfig

	// Bazel holds optional Bazel-specific settings for CI workloads.
	Bazel BazelConfig
}
