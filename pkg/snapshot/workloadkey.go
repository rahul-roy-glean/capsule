package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// SnapshotCommand describes a single warmup step baked into a snapshot.
type SnapshotCommand struct {
	Type      string   `json:"type"` // "shell", "gcp-auth", "exec"
	Args      []string `json:"args"`
	RunAsRoot bool     `json:"run_as_root"` // if true, run as root; otherwise run as the configured runner user
}

// ComputeWorkloadKey returns a stable 16-char hex key. Commands are sorted
// canonically before hashing so insertion order does not matter.
func ComputeWorkloadKey(commands []SnapshotCommand) string {
	sorted := make([]SnapshotCommand, len(commands))
	copy(sorted, commands)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Type != sorted[j].Type {
			return sorted[i].Type < sorted[j].Type
		}
		if sorted[i].RunAsRoot != sorted[j].RunAsRoot {
			// false < true
			return !sorted[i].RunAsRoot
		}
		a, _ := json.Marshal(sorted[i].Args)
		b, _ := json.Marshal(sorted[j].Args)
		return string(a) < string(b)
	})
	b, _ := json.Marshal(sorted)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}

// ComputeLayerHash returns a full 64-char SHA256 hex hash for a layer.
// The hash is computed from parentLayerHash + canonical(initCommands) + canonical(drives) + tier.
// Commands are sorted using the same canonical sort as ComputeWorkloadKey.
// Drives are sorted by DriveID. Tier is included because it determines
// the snapshot's vCPU/memory configuration (baked into Firecracker snapshots).
func ComputeLayerHash(parentLayerHash string, initCommands []SnapshotCommand, drives []DriveSpec, tier string) string {
	sortedCmds := make([]SnapshotCommand, len(initCommands))
	copy(sortedCmds, initCommands)
	sort.Slice(sortedCmds, func(i, j int) bool {
		if sortedCmds[i].Type != sortedCmds[j].Type {
			return sortedCmds[i].Type < sortedCmds[j].Type
		}
		if sortedCmds[i].RunAsRoot != sortedCmds[j].RunAsRoot {
			return !sortedCmds[i].RunAsRoot
		}
		a, _ := json.Marshal(sortedCmds[i].Args)
		b, _ := json.Marshal(sortedCmds[j].Args)
		return string(a) < string(b)
	})

	sortedDrives := make([]DriveSpec, len(drives))
	copy(sortedDrives, drives)
	sort.Slice(sortedDrives, func(i, j int) bool {
		return sortedDrives[i].DriveID < sortedDrives[j].DriveID
	})

	cmdsJSON, _ := json.Marshal(sortedCmds)
	drivesJSON, _ := json.Marshal(sortedDrives)

	h := sha256.Sum256([]byte(parentLayerHash + string(cmdsJSON) + string(drivesJSON) + tier))
	return hex.EncodeToString(h[:])
}

// ComputeLeafWorkloadKey returns a 16-char hex key for the leaf layer.
// This becomes the primary workload_key used by runner allocation,
// version assignments, and all existing machinery.
func ComputeLeafWorkloadKey(leafLayerHash string) string {
	h := sha256.Sum256([]byte("leaf:" + leafLayerHash))
	return hex.EncodeToString(h[:])[:16]
}

// ComputeDerivedWorkloadKey returns a stable 16-char hex key for a derived
// workload. DriveSpecs are sorted by DriveID before hashing so insertion order
// does not matter.
func ComputeDerivedWorkloadKey(baseKey string, driveSpecs []DriveSpec) string {
	sorted := make([]DriveSpec, len(driveSpecs))
	copy(sorted, driveSpecs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].DriveID < sorted[j].DriveID
	})
	b, _ := json.Marshal(sorted)
	h := sha256.Sum256([]byte(baseKey + string(b)))
	return hex.EncodeToString(h[:])[:16]
}
