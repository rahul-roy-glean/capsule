package snapshot

import (
	"testing"
)

func TestComputeLayerHash_Deterministic(t *testing.T) {
	cmds := []SnapshotCommand{
		{Type: "shell", Args: []string{"echo", "hello"}},
		{Type: "shell", Args: []string{"bash", "-c", "git clone --depth=1 https://github.com/org/repo /workspace"}},
	}
	drives := []DriveSpec{
		{DriveID: "data", SizeGB: 10},
	}

	h1 := ComputeLayerHash("", cmds, drives, "standard")
	h2 := ComputeLayerHash("", cmds, drives, "standard")

	if h1 != h2 {
		t.Errorf("ComputeLayerHash not deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("ComputeLayerHash should return 64-char hex, got %d chars", len(h1))
	}
}

func TestComputeLayerHash_OrderIndependent(t *testing.T) {
	cmds1 := []SnapshotCommand{
		{Type: "shell", Args: []string{"echo", "hello"}},
		{Type: "shell", Args: []string{"bash", "-c", "git clone --depth=1 https://github.com/org/repo /workspace"}},
	}
	cmds2 := []SnapshotCommand{
		{Type: "shell", Args: []string{"bash", "-c", "git clone --depth=1 https://github.com/org/repo /workspace"}},
		{Type: "shell", Args: []string{"echo", "hello"}},
	}

	h1 := ComputeLayerHash("parent123", cmds1, nil, "")
	h2 := ComputeLayerHash("parent123", cmds2, nil, "")

	if h1 != h2 {
		t.Errorf("ComputeLayerHash should be order-independent: %s != %s", h1, h2)
	}
}

func TestComputeLayerHash_DifferentInputs(t *testing.T) {
	cmds1 := []SnapshotCommand{
		{Type: "shell", Args: []string{"echo", "hello"}},
	}
	cmds2 := []SnapshotCommand{
		{Type: "shell", Args: []string{"echo", "world"}},
	}

	h1 := ComputeLayerHash("", cmds1, nil, "")
	h2 := ComputeLayerHash("", cmds2, nil, "")

	if h1 == h2 {
		t.Errorf("Different commands should produce different hashes")
	}
}

func TestComputeLayerHash_ParentAffectsHash(t *testing.T) {
	cmds := []SnapshotCommand{
		{Type: "shell", Args: []string{"echo", "hello"}},
	}

	h1 := ComputeLayerHash("", cmds, nil, "")
	h2 := ComputeLayerHash("parentABC", cmds, nil, "")

	if h1 == h2 {
		t.Errorf("Different parent hashes should produce different layer hashes")
	}
}

func TestComputeLayerHash_DrivesAffectHash(t *testing.T) {
	cmds := []SnapshotCommand{
		{Type: "shell", Args: []string{"echo", "hello"}},
	}
	drives1 := []DriveSpec{{DriveID: "a", SizeGB: 10}}
	drives2 := []DriveSpec{{DriveID: "b", SizeGB: 10}}

	h1 := ComputeLayerHash("", cmds, drives1, "")
	h2 := ComputeLayerHash("", cmds, drives2, "")

	if h1 == h2 {
		t.Errorf("Different drives should produce different hashes")
	}
}

func TestComputeLayerHash_DriveOrderIndependent(t *testing.T) {
	cmds := []SnapshotCommand{{Type: "shell", Args: []string{"echo"}}}
	drives1 := []DriveSpec{{DriveID: "a", SizeGB: 10}, {DriveID: "b", SizeGB: 20}}
	drives2 := []DriveSpec{{DriveID: "b", SizeGB: 20}, {DriveID: "a", SizeGB: 10}}

	h1 := ComputeLayerHash("", cmds, drives1, "")
	h2 := ComputeLayerHash("", cmds, drives2, "")

	if h1 != h2 {
		t.Errorf("Drive order should not affect hash: %s != %s", h1, h2)
	}
}

func TestComputeLayerHash_TierAffectsHash(t *testing.T) {
	cmds := []SnapshotCommand{
		{Type: "shell", Args: []string{"echo", "hello"}},
	}

	h1 := ComputeLayerHash("", cmds, nil, "small")
	h2 := ComputeLayerHash("", cmds, nil, "large")

	if h1 == h2 {
		t.Errorf("Different tiers should produce different hashes")
	}
}

func TestComputeLeafWorkloadKey(t *testing.T) {
	key1 := ComputeLeafWorkloadKey("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	if len(key1) != 16 {
		t.Errorf("ComputeLeafWorkloadKey should return 16-char key, got %d", len(key1))
	}

	// Same input should give same output
	key2 := ComputeLeafWorkloadKey("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	if key1 != key2 {
		t.Errorf("ComputeLeafWorkloadKey not deterministic: %s != %s", key1, key2)
	}

	// Different input should give different output
	key3 := ComputeLeafWorkloadKey("1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	if key1 == key3 {
		t.Errorf("Different inputs should produce different leaf keys")
	}
}
