package snapshot

import (
	"testing"
)

func TestValidateLayeredConfig_Valid(t *testing.T) {
	cfg := &LayeredConfig{
		DisplayName: "test",
		Layers: []LayerDef{
			{
				Name:         "base",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"echo", "base"}}},
			},
			{
				Name:            "app",
				InitCommands:    []SnapshotCommand{{Type: "shell", Args: []string{"echo", "app"}}},
				RefreshCommands: []SnapshotCommand{{Type: "shell", Args: []string{"git", "pull"}}},
				RefreshInterval: "6h",
			},
		},
	}
	if err := ValidateLayeredConfig(cfg); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateLayeredConfig_NoLayers(t *testing.T) {
	cfg := &LayeredConfig{DisplayName: "test"}
	if err := ValidateLayeredConfig(cfg); err == nil {
		t.Error("expected error for empty layers")
	}
}

func TestValidateLayeredConfig_DuplicateLayerNames(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "base", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
			{Name: "base", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"b"}}}},
		},
	}
	if err := ValidateLayeredConfig(cfg); err == nil {
		t.Error("expected error for duplicate layer names")
	}
}

func TestValidateLayeredConfig_EmptyInitCommands(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "base"},
		},
	}
	if err := ValidateLayeredConfig(cfg); err == nil {
		t.Error("expected error for empty init_commands")
	}
}

func TestValidateLayeredConfig_InvalidRefreshInterval(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{
				Name:            "base",
				InitCommands:    []SnapshotCommand{{Type: "shell", Args: []string{"a"}}},
				RefreshInterval: "not_valid",
			},
		},
	}
	if err := ValidateLayeredConfig(cfg); err == nil {
		t.Error("expected error for invalid refresh_interval")
	}
}

func TestValidateLayeredConfig_OnPushInterval(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{
				Name:            "base",
				InitCommands:    []SnapshotCommand{{Type: "shell", Args: []string{"a"}}},
				RefreshInterval: "on_push",
			},
		},
	}
	if err := ValidateLayeredConfig(cfg); err != nil {
		t.Errorf("on_push should be valid: %v", err)
	}
}

func TestValidateLayeredConfig_DuplicateDriveIDs(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{
				Name:         "base",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}},
				Drives:       []DriveSpec{{DriveID: "data"}},
			},
			{
				Name:         "app",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"b"}}},
				Drives:       []DriveSpec{{DriveID: "data"}},
			},
		},
	}
	if err := ValidateLayeredConfig(cfg); err == nil {
		t.Error("expected error for duplicate drive IDs")
	}
}

func TestValidateLayeredConfig_EmptyLayerName(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{
				Name:         "",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}},
			},
		},
	}
	if err := ValidateLayeredConfig(cfg); err == nil {
		t.Error("expected error for empty layer name")
	}
}

func TestMaterializeLayers_SingleLayer(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{
				Name:         "base",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"echo"}}},
			},
		},
	}

	layers := MaterializeLayers(cfg)
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	if layers[0].ParentLayerHash != "" {
		t.Error("root layer should have empty parent hash")
	}
	if layers[0].Depth != 0 {
		t.Errorf("root layer depth should be 0, got %d", layers[0].Depth)
	}
	if layers[0].LayerHash == "" {
		t.Error("layer hash should not be empty")
	}
}

func TestMaterializeLayers_Chain(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "base", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
			{Name: "mid", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"b"}}}},
			{Name: "leaf", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"c"}}}},
		},
	}

	layers := MaterializeLayers(cfg)
	if len(layers) != 3 {
		t.Fatalf("expected 3 layers, got %d", len(layers))
	}

	// Verify chain
	if layers[0].ParentLayerHash != "" {
		t.Error("layer 0 should have no parent")
	}
	if layers[1].ParentLayerHash != layers[0].LayerHash {
		t.Error("layer 1's parent should be layer 0")
	}
	if layers[2].ParentLayerHash != layers[1].LayerHash {
		t.Error("layer 2's parent should be layer 1")
	}

	// Verify depths
	for i, l := range layers {
		if l.Depth != i {
			t.Errorf("layer %d depth should be %d, got %d", i, i, l.Depth)
		}
	}

	// All hashes should be unique
	hashes := map[string]bool{}
	for _, l := range layers {
		if hashes[l.LayerHash] {
			t.Errorf("duplicate layer hash: %s", l.LayerHash)
		}
		hashes[l.LayerHash] = true
	}
}

func TestMaterializeLayers_Deterministic(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "base", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
			{Name: "leaf", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"b"}}}},
		},
	}

	layers1 := MaterializeLayers(cfg)
	layers2 := MaterializeLayers(cfg)

	for i := range layers1 {
		if layers1[i].LayerHash != layers2[i].LayerHash {
			t.Errorf("layer %d hash not deterministic: %s != %s", i, layers1[i].LayerHash, layers2[i].LayerHash)
		}
	}
}

func TestMaterializeLayers_DifferentConfigs(t *testing.T) {
	cfg1 := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "base", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
			{Name: "leaf", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"b"}}}},
		},
	}
	cfg2 := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "base", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
			{Name: "leaf", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"c"}}}},
		},
	}

	layers1 := MaterializeLayers(cfg1)
	layers2 := MaterializeLayers(cfg2)

	// Shared base layer should have same hash
	if layers1[0].LayerHash != layers2[0].LayerHash {
		t.Error("shared base layer should have same hash")
	}

	// Different leaf should have different hash
	if layers1[1].LayerHash == layers2[1].LayerHash {
		t.Error("different leaf layers should have different hashes")
	}
}

func TestMaterializeLayers_BaseImage_InjectsPlatformLayer(t *testing.T) {
	cfg := &LayeredConfig{
		BaseImage: "ubuntu:22.04",
		Layers: []LayerDef{
			{Name: "app", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"echo", "hi"}}}},
		},
	}

	layers := MaterializeLayers(cfg)
	if len(layers) != 2 {
		t.Fatalf("expected 2 layers (platform + app), got %d", len(layers))
	}

	// Layer 0 should be the platform layer
	if layers[0].Name != PlatformLayerName {
		t.Errorf("layer 0 name should be %q, got %q", PlatformLayerName, layers[0].Name)
	}
	if layers[0].BaseImage != "ubuntu:22.04" {
		t.Errorf("layer 0 BaseImage should be 'ubuntu:22.04', got %q", layers[0].BaseImage)
	}
	if layers[0].Depth != 0 {
		t.Errorf("platform layer depth should be 0, got %d", layers[0].Depth)
	}

	// Layer 1 should be the user layer
	if layers[1].Name != "app" {
		t.Errorf("layer 1 name should be 'app', got %q", layers[1].Name)
	}
	if layers[1].ParentLayerHash != layers[0].LayerHash {
		t.Error("user layer's parent should be the platform layer")
	}
	if layers[1].Depth != 1 {
		t.Errorf("user layer depth should be 1, got %d", layers[1].Depth)
	}
}

func TestMaterializeLayers_NoBaseImage_NoInjection(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "base", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
		},
	}

	layers := MaterializeLayers(cfg)
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer (no injection without BaseImage), got %d", len(layers))
	}
	if layers[0].Name != "base" {
		t.Errorf("expected user layer, got %q", layers[0].Name)
	}
}

func TestMaterializeLayers_DifferentBaseImages_DifferentHashes(t *testing.T) {
	cfg1 := &LayeredConfig{
		BaseImage: "ubuntu:22.04",
		Layers: []LayerDef{
			{Name: "app", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
		},
	}
	cfg2 := &LayeredConfig{
		BaseImage: "debian:bookworm",
		Layers: []LayerDef{
			{Name: "app", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
		},
	}

	layers1 := MaterializeLayers(cfg1)
	layers2 := MaterializeLayers(cfg2)

	// Platform layers should differ (different base image)
	if layers1[0].LayerHash == layers2[0].LayerHash {
		t.Error("different base images should produce different platform layer hashes")
	}

	// User layers should also differ (different parent hash)
	if layers1[1].LayerHash == layers2[1].LayerHash {
		t.Error("user layers should differ because parent platform hashes differ")
	}
}

func TestMaterializeLayers_SameBaseImage_SharedPlatform(t *testing.T) {
	cfg1 := &LayeredConfig{
		BaseImage: "ubuntu:22.04",
		Layers: []LayerDef{
			{Name: "app1", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
		},
	}
	cfg2 := &LayeredConfig{
		BaseImage: "ubuntu:22.04",
		Layers: []LayerDef{
			{Name: "app2", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"b"}}}},
		},
	}

	layers1 := MaterializeLayers(cfg1)
	layers2 := MaterializeLayers(cfg2)

	// Platform layers should be identical (same base image, same commands)
	if layers1[0].LayerHash != layers2[0].LayerHash {
		t.Error("same base image should produce identical platform layer hashes")
	}
}

func TestMaterializeLayers_AutoInjectsWorkspaceDrive(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{
				Name:         "app",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"build"}}},
				// No drives — should get auto-injected workspace drive
			},
		},
	}

	layers := MaterializeLayers(cfg)
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	if len(layers[0].Drives) != 1 {
		t.Fatalf("expected 1 auto-injected drive, got %d", len(layers[0].Drives))
	}
	d := layers[0].Drives[0]
	if d.DriveID != "workspace" {
		t.Errorf("expected drive ID 'workspace', got %q", d.DriveID)
	}
	if d.Label != "WORKSPACE" {
		t.Errorf("expected label 'WORKSPACE', got %q", d.Label)
	}
	if d.SizeGB != 50 {
		t.Errorf("expected default size 50GB, got %d", d.SizeGB)
	}
	if d.MountPath != "/workspace" {
		t.Errorf("expected mount path '/workspace', got %q", d.MountPath)
	}
}

func TestMaterializeLayers_WorkspaceSizeGBConfig(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{
				Name:         "app",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"build"}}},
			},
		},
	}
	cfg.Config.WorkspaceSizeGB = 100

	layers := MaterializeLayers(cfg)
	if len(layers[0].Drives) != 1 {
		t.Fatalf("expected 1 auto-injected drive, got %d", len(layers[0].Drives))
	}
	if layers[0].Drives[0].SizeGB != 100 {
		t.Errorf("expected configured size 100GB, got %d", layers[0].Drives[0].SizeGB)
	}
}

func TestMaterializeLayers_NoDriveInjectionIfDrivesExist(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{
				Name:         "app",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"build"}}},
				Drives:       []DriveSpec{{DriveID: "data", SizeGB: 20}},
			},
		},
	}

	layers := MaterializeLayers(cfg)
	if len(layers[0].Drives) != 1 {
		t.Fatalf("expected 1 drive (user-specified), got %d", len(layers[0].Drives))
	}
	if layers[0].Drives[0].DriveID != "data" {
		t.Errorf("expected user-specified drive 'data', got %q", layers[0].Drives[0].DriveID)
	}
}

func TestMaterializeLayers_NoDriveInjectionForPlatformLayer(t *testing.T) {
	cfg := &LayeredConfig{
		BaseImage: "ubuntu:22.04",
		Layers: []LayerDef{
			{
				Name:         "app",
				InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"build"}}},
			},
		},
	}

	layers := MaterializeLayers(cfg)
	if len(layers) != 2 {
		t.Fatalf("expected 2 layers, got %d", len(layers))
	}
	// Platform layer should have no drives
	if len(layers[0].Drives) != 0 {
		t.Errorf("platform layer should have no auto-injected drives, got %d", len(layers[0].Drives))
	}
	// User layer should have auto-injected workspace drive
	if len(layers[1].Drives) != 1 {
		t.Fatalf("user layer should have 1 auto-injected drive, got %d", len(layers[1].Drives))
	}
	if layers[1].Drives[0].DriveID != "workspace" {
		t.Errorf("expected drive ID 'workspace', got %q", layers[1].Drives[0].DriveID)
	}
}

func TestMaterializeLayers_WorkspaceDriveInHash(t *testing.T) {
	// Auto-injected workspace drive should be part of the hash
	cfg1 := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "app", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"build"}}}},
		},
	}
	cfg2 := &LayeredConfig{
		Layers: []LayerDef{
			{Name: "app", InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"build"}}}},
		},
	}
	cfg2.Config.WorkspaceSizeGB = 100

	layers1 := MaterializeLayers(cfg1)
	layers2 := MaterializeLayers(cfg2)

	// Different workspace sizes should produce different hashes
	if layers1[0].LayerHash == layers2[0].LayerHash {
		t.Error("different workspace sizes should produce different layer hashes")
	}
}

func TestValidateLayeredConfig_ReservedPlatformName(t *testing.T) {
	cfg := &LayeredConfig{
		Layers: []LayerDef{
			{Name: PlatformLayerName, InitCommands: []SnapshotCommand{{Type: "shell", Args: []string{"a"}}}},
		},
	}
	if err := ValidateLayeredConfig(cfg); err == nil {
		t.Error("expected error for reserved platform layer name")
	}
}
