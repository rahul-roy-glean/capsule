package main

import (
	"testing"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

func TestLeafKeyMatchesLayeredConfigMaterialization(t *testing.T) {
	commands := []snapshot.SnapshotCommand{
		{Type: "shell", Args: []string{"echo", "dev-snapshot-ready"}},
	}

	cfg := &snapshot.LayeredConfig{
		DisplayName: "test",
		Layers: []snapshot.LayerDef{
			{
				Name:         "base",
				InitCommands: commands,
			},
		},
	}

	layers := snapshot.MaterializeLayers(cfg)
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}

	got := snapshot.ComputeLeafWorkloadKey(layers[0].LayerHash)
	if got != "356613a133ab095a" {
		t.Fatalf("leaf workload key = %s, want %s", got, "356613a133ab095a")
	}
}
