package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/rahul-roy-glean/capsule/pkg/authproxy"
)

// PlatformLayerName is the reserved name for the auto-injected platform layer.
const PlatformLayerName = "_platform"

var validConfigID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// ValidateConfigID checks that a config_id is a valid slug:
// lowercase alphanumeric with hyphens, 3-64 characters, no leading/trailing hyphens.
func ValidateConfigID(id string) error {
	if len(id) < 3 || len(id) > 64 {
		return fmt.Errorf("config_id must be 3-64 characters, got %d", len(id))
	}
	if !validConfigID.MatchString(id) {
		return fmt.Errorf("config_id must be lowercase alphanumeric with hyphens (no leading/trailing hyphens)")
	}
	return nil
}

// LayerDef defines a single layer in a layered snapshot config.
type LayerDef struct {
	Name            string            `json:"name" yaml:"name"`
	InitCommands    []SnapshotCommand `json:"init_commands" yaml:"init_commands"`
	RefreshCommands []SnapshotCommand `json:"refresh_commands,omitempty" yaml:"refresh_commands"`
	Drives          []DriveSpec       `json:"drives,omitempty" yaml:"drives"`
	RefreshInterval string            `json:"refresh_interval,omitempty" yaml:"refresh_interval"` // "6h", "on_push", "daily"
}

// LayeredConfig describes a multi-layer snapshot pipeline.
type LayeredConfig struct {
	DisplayName string     `json:"display_name" yaml:"display_name"`
	BaseImage   string     `json:"base_image,omitempty" yaml:"base_image"` // Docker image URI (e.g. "ubuntu:22.04", "us-docker.pkg.dev/proj/repo/img:tag")
	Layers      []LayerDef `json:"layers" yaml:"layers"`
	Config      struct {
		AutoPause            bool                  `json:"auto_pause,omitempty" yaml:"auto_pause"`
		TTL                  int                   `json:"ttl,omitempty" yaml:"ttl"`
		Tier                 string                `json:"tier,omitempty" yaml:"tier"`
		AutoRollout          bool                  `json:"auto_rollout,omitempty" yaml:"auto_rollout"`
		SessionMaxAgeSeconds int                   `json:"session_max_age_seconds,omitempty" yaml:"session_max_age_seconds"`
		RootfsSizeGB         int                   `json:"rootfs_size_gb,omitempty" yaml:"rootfs_size_gb"`       // rootfs size for layer 0 (default 8)
		RunnerUser           string                `json:"runner_user,omitempty" yaml:"runner_user"`             // user for non-root commands (default "runner")
		WorkspaceSizeGB      int                   `json:"workspace_size_gb,omitempty" yaml:"workspace_size_gb"` // auto-injected workspace drive size (default 50)
		NetworkPolicyPreset  string                `json:"network_policy_preset,omitempty" yaml:"network_policy_preset"`
		NetworkPolicy        json.RawMessage       `json:"network_policy,omitempty" yaml:"network_policy"`
		Auth                 *authproxy.AuthConfig `json:"auth,omitempty" yaml:"auth"`
	} `json:"config" yaml:"config"`
	StartCommand *StartCommand `json:"start_command,omitempty" yaml:"start_command"`
}

// LayerMaterialized is a LayerDef with computed hash chain info.
type LayerMaterialized struct {
	LayerDef
	LayerHash       string      `json:"layer_hash"`
	ParentLayerHash string      `json:"parent_layer_hash"`          // "" for root
	Depth           int         `json:"depth"`                      // 0 for root
	BaseImage       string      `json:"base_image,omitempty"`       // only set on the platform layer (depth 0)
	AllChainDrives  []DriveSpec `json:"all_chain_drives,omitempty"` // union of all drives across all layers in the config
}

// ValidateLayeredConfig checks a LayeredConfig for correctness.
func ValidateLayeredConfig(cfg *LayeredConfig) error {
	if len(cfg.Layers) == 0 && cfg.BaseImage == "" {
		return fmt.Errorf("at least one layer is required when no base_image is set")
	}

	names := make(map[string]bool)
	driveIDs := make(map[string]string) // driveID -> layer name
	for _, layer := range cfg.Layers {
		if layer.Name == "" {
			return fmt.Errorf("layer name must not be empty")
		}
		if layer.Name == PlatformLayerName {
			return fmt.Errorf("layer name %q is reserved for the system platform layer", PlatformLayerName)
		}
		if names[layer.Name] {
			return fmt.Errorf("duplicate layer name: %s", layer.Name)
		}
		names[layer.Name] = true

		if len(layer.InitCommands) == 0 {
			return fmt.Errorf("layer %q must have non-empty init_commands", layer.Name)
		}

		if layer.RefreshInterval != "" {
			if err := validateRefreshInterval(layer.RefreshInterval); err != nil {
				return fmt.Errorf("layer %q: invalid refresh_interval: %w", layer.Name, err)
			}
		}

		for _, d := range layer.Drives {
			if prev, ok := driveIDs[d.DriveID]; ok {
				return fmt.Errorf("duplicate drive ID %q in layers %q and %q", d.DriveID, prev, layer.Name)
			}
			driveIDs[d.DriveID] = layer.Name
		}
	}

	return nil
}

// validateRefreshInterval checks that a refresh_interval value is valid.
func validateRefreshInterval(interval string) error {
	_, err := time.ParseDuration(interval)
	if err != nil {
		return fmt.Errorf("%q is not a valid duration", interval)
	}
	return nil
}

// MaterializeLayers computes the hash chain for a LayeredConfig.
// If BaseImage is set, an implicit platform layer (depth 0) is prepended that
// converts the Docker image to a Firecracker rootfs and installs capsule-thaw-agent.
// User layers follow at depth 1+.
func MaterializeLayers(cfg *LayeredConfig) []LayerMaterialized {
	var allLayers []LayerDef
	hasBaseImage := cfg.BaseImage != ""

	if hasBaseImage {
		allLayers = append(allLayers, buildPlatformLayerDef(cfg))
	}
	allLayers = append(allLayers, cfg.Layers...)

	// Auto-inject workspace drive for user layers (not _platform) that have no drives.
	for i := range allLayers {
		if allLayers[i].Name != PlatformLayerName && len(allLayers[i].Drives) == 0 {
			sizeGB := 50
			if cfg.Config.WorkspaceSizeGB > 0 {
				sizeGB = cfg.Config.WorkspaceSizeGB
			}
			allLayers[i].Drives = []DriveSpec{
				{DriveID: "workspace", Label: "WORKSPACE", SizeGB: sizeGB, MountPath: "/workspace"},
			}
		}
	}

	result := make([]LayerMaterialized, len(allLayers))
	parentHash := ""
	tier := cfg.Config.Tier

	for i, layer := range allLayers {
		layerHash := ComputeLayerHash(parentHash, layer.InitCommands, layer.Drives, tier)
		result[i] = LayerMaterialized{
			LayerDef:        layer,
			LayerHash:       layerHash,
			ParentLayerHash: parentHash,
			Depth:           i,
		}
		if i == 0 && hasBaseImage {
			result[i].BaseImage = cfg.BaseImage
		}
		parentHash = layerHash
	}

	// Compute the union of all drives across all layers.
	// Every layer gets the same set so that Firecracker snapshot restore
	// works — you can't add/remove drives between snapshot and restore.
	seen := make(map[string]bool)
	var allDrives []DriveSpec
	for _, layer := range result {
		for _, d := range layer.Drives {
			if !seen[d.DriveID] {
				seen[d.DriveID] = true
				allDrives = append(allDrives, d)
			}
		}
	}
	for i := range result {
		result[i].AllChainDrives = allDrives
	}

	return result
}

// buildPlatformLayerDef creates the implicit platform layer definition.
// This layer converts a Docker image to a Firecracker rootfs and installs
// the minimal system components: systemd init, capsule-thaw-agent, networking config.
//
// The hash for this layer includes the base_image URI, so changing the
// Docker image triggers a rebuild while keeping user layers' hashes stable
// if their commands don't change.
func buildPlatformLayerDef(cfg *LayeredConfig) LayerDef {
	runnerUser := cfg.Config.RunnerUser
	if runnerUser == "" {
		runnerUser = "runner"
	}

	// The platform layer's init_commands describe what the snapshot-builder
	// does internally when --base-image is set. They're included in the hash
	// so that changes to the platform setup (e.g., new capsule-thaw-agent version)
	// produce a different layer hash and trigger a rebuild.
	//
	// These are NOT executed as shell commands inside the VM — they're
	// declarative markers processed by the snapshot-builder's rootfs setup.
	return LayerDef{
		Name: PlatformLayerName,
		InitCommands: []SnapshotCommand{
			{Type: "base-image", Args: []string{cfg.BaseImage}},
			{Type: "platform-setup", Args: []string{"capsule-thaw-agent", "systemd", "networking", "docker-env-v2"}, RunAsRoot: true},
			{Type: "platform-user", Args: []string{runnerUser}},
		},
	}
}

// ComputeBaseImageHash returns a hash for a base image URI.
// Used to include the image identity in the layer hash chain.
func ComputeBaseImageHash(imageURI string) string {
	h := sha256.Sum256([]byte("base-image:" + imageURI))
	return hex.EncodeToString(h[:])
}
