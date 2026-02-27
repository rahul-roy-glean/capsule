package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// SnapshotCommand describes a single warmup step baked into a snapshot.
type SnapshotCommand struct {
	Type      string   `json:"type"` // "git-clone", "gcp-auth", "shell"
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
