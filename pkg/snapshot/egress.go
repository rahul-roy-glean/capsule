package snapshot

import (
	"sync/atomic"
)

// EgressBudget tracks per-runner GCS egress and enforces soft/hard caps.
// The soft cap disables prefetch for the runner; the hard cap triggers VM kill.
type EgressBudget struct {
	bytesFromGCS atomic.Int64
	softCapBytes int64
	hardCapBytes int64
}

// NewEgressBudget creates a new egress budget.
// softCap: disable prefetch when exceeded (default 4GB).
// hardCap: kill runner when exceeded (default 16GB).
func NewEgressBudget(softCap, hardCap int64) *EgressBudget {
	if softCap <= 0 {
		softCap = 4 * 1024 * 1024 * 1024 // 4GB
	}
	if hardCap <= 0 {
		hardCap = 16 * 1024 * 1024 * 1024 // 16GB
	}
	return &EgressBudget{
		softCapBytes: softCap,
		hardCapBytes: hardCap,
	}
}

// Add records bytes fetched from GCS and returns the new total.
func (b *EgressBudget) Add(n int64) int64 {
	return b.bytesFromGCS.Add(n)
}

// Total returns total bytes fetched from GCS.
func (b *EgressBudget) Total() int64 {
	return b.bytesFromGCS.Load()
}

// SoftCapExceeded returns true if the soft cap has been exceeded.
func (b *EgressBudget) SoftCapExceeded() bool {
	return b.bytesFromGCS.Load() >= b.softCapBytes
}

// HardCapExceeded returns true if the hard cap has been exceeded.
func (b *EgressBudget) HardCapExceeded() bool {
	return b.bytesFromGCS.Load() >= b.hardCapBytes
}

// SoftCap returns the soft cap in bytes.
func (b *EgressBudget) SoftCap() int64 { return b.softCapBytes }

// HardCap returns the hard cap in bytes.
func (b *EgressBudget) HardCap() int64 { return b.hardCapBytes }
