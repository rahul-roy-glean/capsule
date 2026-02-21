package runner

import (
	"net"
	"time"
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
)

// Runner represents a single Bazel runner instance
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
	RepoSlug                string // Deterministic repo slug for multi-repo support
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

	// Pool-related fields
	PoolKey          *RunnerKey `json:"pool_key,omitempty"`
	PausedAt         time.Time  `json:"paused_at,omitempty"`
	TaskCount        int        `json:"task_count"`
	MemoryUsageBytes int64      `json:"memory_usage_bytes,omitempty"`
	DiskUsageBytes   int64      `json:"disk_usage_bytes,omitempty"`
}

// Resources represents the resources allocated to a runner
type Resources struct {
	VCPUs    int
	MemoryMB int
	DiskGB   int
}

// AllocateRequest represents a request to allocate a runner
type AllocateRequest struct {
	RequestID         string
	Repo              string
	Branch            string
	Commit            string
	RepoSlug          string // Deterministic repo identifier for multi-repo support
	Resources         Resources
	Labels            map[string]string
	GitHubRunnerToken string
	CISystem          string // CI system identifier
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

// HostConfig holds configuration for the host agent
type HostConfig struct {
	HostID            string
	InstanceName      string
	Zone              string
	MaxRunners        int
	IdleTarget        int
	VCPUsPerRunner    int
	MemoryMBPerRunner int
	FirecrackerBin    string
	SocketDir         string
	WorkspaceDir      string
	LogDir            string
	SnapshotBucket    string
	SnapshotCachePath string
	// RepoCacheUpperSizeGB controls the per-runner writable layer size for the
	// Bazel repository cache overlay.
	RepoCacheUpperSizeGB int
	// BuildbarnCertsDir is a host directory containing Buildbarn certificates
	// (e.g. ca.crt, client.crt, client.pem). If set, the host agent will package
	// this directory into an ext4 image and attach it read-only to each microVM.
	BuildbarnCertsDir string
	// BuildbarnCertsMountPath is where the certs will be mounted inside the microVM.
	BuildbarnCertsMountPath string
	// BuildbarnCertsImageSizeMB controls the size of the generated ext4 image.
	BuildbarnCertsImageSizeMB int
	// Credentials configuration (generic replacement for buildbarn-specific certs)
	// CredentialsSecrets lists secrets to fetch from GCP Secret Manager
	CredentialsSecrets []CredentialSecret
	// CredentialsHostDirs lists host directories to include in the credentials drive
	CredentialsHostDirs []CredentialHostDir
	// CredentialsEnv maps environment variable names to their values (from credentials drive)
	CredentialsEnv map[string]string
	// CredentialsImageSizeMB controls the size of the generated credentials ext4 image
	CredentialsImageSizeMB int
	// QuarantineDir is where the host will write quarantine manifests and keep
	// per-runner debug metadata when a runner is quarantined.
	QuarantineDir     string
	MicroVMSubnet     string
	ExternalInterface string
	BridgeName        string
	Environment       string
	ControlPlaneAddr  string

	// Runner Pool Configuration
	PoolEnabled            bool `json:"pool_enabled"`
	PoolMaxRunners         int  `json:"pool_max_runners"`
	PoolMaxTotalMemoryGB   int  `json:"pool_max_total_memory_gb"`
	PoolMaxRunnerMemoryGB  int  `json:"pool_max_runner_memory_gb"`
	PoolMaxRunnerDiskGB    int  `json:"pool_max_runner_disk_gb"`
	PoolRecycleTimeoutSecs int  `json:"pool_recycle_timeout_secs"`

	// GitCacheEnabled enables git-cache reference cloning for faster repo setup
	GitCacheEnabled bool
	// GitCacheDir is the host directory containing git mirrors (e.g. /mnt/nvme/git-cache)
	GitCacheDir string
	// GitCacheImagePath is the path to the git-cache block device image
	GitCacheImagePath string
	// GitCacheMountPath is where the git-cache will be mounted inside microVMs
	GitCacheMountPath string
	// GitCacheRepoMappings maps repo identifiers to their cache directory names
	// e.g. {"github.com/askscio/scio": "scio"} means cache is at GitCacheDir/scio
	GitCacheRepoMappings map[string]string
	// GitCacheWorkspaceDir is the target directory for cloned repos inside microVMs
	GitCacheWorkspaceDir string
	// GitCachePreClonedPath is the path where repos were pre-cloned during warmup
	// (baked into the snapshot rootfs). Defaults to /workspace if not set.
	GitCachePreClonedPath string

	// GitHub Runner Registration (Option C: pre-register at boot)
	// GitHubRunnerEnabled enables automatic runner registration at VM boot
	GitHubRunnerEnabled bool
	// GitHubRepo is the repository runners will register to (e.g., "askscio/scio")
	GitHubRepo string
	// GitHubOrg is the GitHub organization for org-level runner registration
	// If set, uses org-level API which requires "Organization self-hosted runners" permission
	// If empty, uses repo-level API which requires "Administration" permission on the repo
	GitHubOrg string
	// GitHubRunnerLabels are labels applied to registered runners
	GitHubRunnerLabels []string
	// GitHubRunnerEphemeral controls whether runners exit after one job (true) or persist (false)
	GitHubRunnerEphemeral bool
	// GitHubAppID is the GitHub App ID for authentication
	GitHubAppID string
	// GitHubAppSecret is the Secret Manager secret name containing the private key
	GitHubAppSecret string
	// GCPProject is the GCP project for Secret Manager access
	GCPProject string
}
