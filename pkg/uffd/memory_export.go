//go:build linux
// +build linux

package uffd

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ExportDirtyMemory reads dirty memory pages directly from a running process
// using process_vm_readv, bypassing the need for intermediate snapshot files.
// This is significantly faster than the CreateDiffSnapshot + SEEK_DATA/SEEK_HOLE
// path for large VMs (8GB+).
//
// Parameters:
//   - pid: the Firecracker process ID (must be paused)
//   - mappings: guest memory region mappings from UFFD handshake
//   - dirtyOffsets: snapshot file offsets of dirty pages to export
//   - chunkSize: size of chunks for grouping pages (typically 4MB)
//
// Returns a map of page offset to page data.
func ExportDirtyMemory(pid int, mappings []GuestRegionUFFDMapping, dirtyOffsets []int64, chunkSize int64) (map[int64][]byte, error) {
	if len(dirtyOffsets) == 0 {
		return nil, nil
	}

	result := make(map[int64][]byte, len(dirtyOffsets))

	for _, offset := range dirtyOffsets {
		// Find the mapping containing this offset
		var hostAddr uintptr
		found := false
		for i := range mappings {
			m := &mappings[i]
			if uintptr(offset) >= m.Offset && uintptr(offset) < m.Offset+m.Size {
				hostAddr = m.BaseHostVirtAddr + uintptr(offset) - m.Offset
				found = true
				break
			}
		}
		if !found {
			continue // offset not in any mapping, skip
		}

		// Read one page from the remote process
		page := make([]byte, PageSize)
		localIov := []unix.Iovec{
			{
				Base: &page[0],
				Len:  PageSize,
			},
		}
		remoteIov := []RemoteIovec{
			{
				Base: hostAddr,
				Len:  PageSize,
			},
		}

		n, err := processVMReadv(pid, localIov, remoteIov)
		if err != nil {
			return nil, fmt.Errorf("process_vm_readv at offset 0x%x (host 0x%x): %w", offset, hostAddr, err)
		}
		if n != PageSize {
			return nil, fmt.Errorf("process_vm_readv short read: got %d, want %d", n, PageSize)
		}

		result[offset] = page
	}

	return result, nil
}

// ExportDirtyMemoryBatched reads dirty memory pages in batches using
// process_vm_readv with scatter-gather I/O for better performance.
// Pages within the same contiguous region are batched into a single syscall.
func ExportDirtyMemoryBatched(pid int, mappings []GuestRegionUFFDMapping, dirtyOffsets []int64) (map[int64][]byte, error) {
	if len(dirtyOffsets) == 0 {
		return nil, nil
	}

	result := make(map[int64][]byte, len(dirtyOffsets))

	// Process in batches of up to 1024 pages (UIO_MAXIOV on Linux)
	const maxBatch = 1024

	for start := 0; start < len(dirtyOffsets); start += maxBatch {
		end := start + maxBatch
		if end > len(dirtyOffsets) {
			end = len(dirtyOffsets)
		}
		batch := dirtyOffsets[start:end]

		// Allocate contiguous buffer for the batch
		buf := make([]byte, len(batch)*PageSize)
		localIovs := make([]unix.Iovec, len(batch))
		remoteIovs := make([]RemoteIovec, 0, len(batch))

		for i, offset := range batch {
			var hostAddr uintptr
			found := false
			for j := range mappings {
				m := &mappings[j]
				if uintptr(offset) >= m.Offset && uintptr(offset) < m.Offset+m.Size {
					hostAddr = m.BaseHostVirtAddr + uintptr(offset) - m.Offset
					found = true
					break
				}
			}
			if !found {
				continue
			}

			pageStart := i * PageSize
			localIovs[i] = unix.Iovec{
				Base: &buf[pageStart],
				Len:  PageSize,
			}
			remoteIovs = append(remoteIovs, RemoteIovec{
				Base: hostAddr,
				Len:  PageSize,
			})
		}

		if len(remoteIovs) == 0 {
			continue
		}

		_, err := processVMReadv(pid, localIovs[:len(remoteIovs)], remoteIovs)
		if err != nil {
			return nil, fmt.Errorf("process_vm_readv batch: %w", err)
		}

		// Extract individual pages from the buffer
		for i, offset := range batch {
			if i >= len(remoteIovs) {
				break
			}
			pageStart := i * PageSize
			page := make([]byte, PageSize)
			copy(page, buf[pageStart:pageStart+PageSize])
			result[offset] = page
		}
	}

	return result, nil
}

// RemoteIovec represents a remote process memory region for process_vm_readv.
// Layout must match struct iovec: pointer followed by size.
type RemoteIovec struct {
	Base uintptr
	Len  uint64
}

// processVMReadv wraps the process_vm_readv(2) syscall.
func processVMReadv(pid int, localIov []unix.Iovec, remoteIov []RemoteIovec) (int, error) {
	var localIovPtr unsafe.Pointer
	if len(localIov) > 0 {
		localIovPtr = unsafe.Pointer(&localIov[0])
	}
	var remoteIovPtr unsafe.Pointer
	if len(remoteIov) > 0 {
		remoteIovPtr = unsafe.Pointer(&remoteIov[0])
	}

	n, _, errno := unix.Syscall6(
		unix.SYS_PROCESS_VM_READV,
		uintptr(pid),
		uintptr(localIovPtr),
		uintptr(len(localIov)),
		uintptr(remoteIovPtr),
		uintptr(len(remoteIov)),
		0, // flags (must be 0)
	)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}
