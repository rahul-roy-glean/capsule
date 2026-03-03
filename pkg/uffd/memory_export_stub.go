//go:build !linux
// +build !linux

package uffd

import "fmt"

// ExportDirtyMemory is a stub for non-Linux platforms.
func ExportDirtyMemory(pid int, mappings []GuestRegionUFFDMapping, dirtyOffsets []int64, chunkSize int64) (map[int64][]byte, error) {
	return nil, fmt.Errorf("process_vm_readv is only supported on Linux")
}

// ExportDirtyMemoryBatched is a stub for non-Linux platforms.
func ExportDirtyMemoryBatched(pid int, mappings []GuestRegionUFFDMapping, dirtyOffsets []int64) (map[int64][]byte, error) {
	return nil, fmt.Errorf("process_vm_readv is only supported on Linux")
}

// RemoteIovec is a stub for non-Linux platforms.
type RemoteIovec struct {
	Base uintptr
	Len  uint64
}
