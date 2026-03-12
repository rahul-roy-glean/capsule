//go:build linux
// +build linux

package uffd

import (
	"context"
	"sync"
	"unsafe"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

// Prefetcher proactively fetches and installs pages into guest memory based on
// a recorded access-pattern mapping from a previous run. It operates in two phases:
//
// Phase 1 (fetch workers): N goroutines fetch chunks from ChunkStore into the
// in-memory cache. This starts immediately and doesn't require the UFFD connection.
//
// Phase 2 (copy workers): M goroutines wait for the UFFD handler's connected
// channel, then use UFFDIO_COPY to proactively install fetched pages into guest
// memory before the VM faults on them.
type Prefetcher struct {
	mapping    *snapshot.PrefetchMapping
	chunkStore *snapshot.ChunkStore
	metadata   *snapshot.ChunkedSnapshotMetadata

	// connected is closed when the UFFD handler has received its connection
	// from Firecracker. Phase 2 workers wait on this before issuing UFFDIO_COPY.
	connected <-chan struct{}

	// uffdFd and mappings are set after connection and used by copy workers.
	uffdFd   int
	mappings []GuestRegionUFFDMapping

	fetchWorkers int
	copyWorkers  int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	logger *logrus.Entry
}

// PrefetcherConfig holds configuration for the prefetcher.
type PrefetcherConfig struct {
	Mapping    *snapshot.PrefetchMapping
	ChunkStore *snapshot.ChunkStore
	Metadata   *snapshot.ChunkedSnapshotMetadata
	Connected  <-chan struct{}
	Logger     *logrus.Logger

	// FetchWorkers is the number of goroutines fetching chunks. Default: 8.
	FetchWorkers int
	// CopyWorkers is the number of goroutines doing UFFDIO_COPY. Default: 4.
	CopyWorkers int
}

// NewPrefetcher creates a new prefetcher. Call Start() to begin.
func NewPrefetcher(cfg PrefetcherConfig) *Prefetcher {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}
	fetchWorkers := cfg.FetchWorkers
	if fetchWorkers == 0 {
		fetchWorkers = 8
	}
	copyWorkers := cfg.CopyWorkers
	if copyWorkers == 0 {
		copyWorkers = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Prefetcher{
		mapping:      cfg.Mapping,
		chunkStore:   cfg.ChunkStore,
		metadata:     cfg.Metadata,
		connected:    cfg.Connected,
		fetchWorkers: fetchWorkers,
		copyWorkers:  copyWorkers,
		ctx:          ctx,
		cancel:       cancel,
		logger:       cfg.Logger.WithField("component", "uffd-prefetcher"),
	}
}

// prefetchItem carries a fetched page from phase 1 to phase 2.
type prefetchItem struct {
	offset   int64
	pageData []byte
}

// SetUFFD sets the UFFD file descriptor and mappings for phase 2 copy workers.
// Must be called before the connected channel is closed.
func (p *Prefetcher) SetUFFD(uffdFd int, mappings []GuestRegionUFFDMapping) {
	p.uffdFd = uffdFd
	p.mappings = mappings
}

// Start begins both phases of prefetching. Non-blocking.
func (p *Prefetcher) Start() {
	if p.mapping == nil || len(p.mapping.Offsets) == 0 {
		return
	}

	p.logger.WithField("pages", len(p.mapping.Offsets)).Info("Starting prefetcher")

	// Channel between fetch workers (phase 1) and copy workers (phase 2).
	fetchedCh := make(chan prefetchItem, 256)

	// Phase 1: fetch workers — warm the ChunkStore cache.
	offsetCh := make(chan int64, 256)

	// Use a separate WaitGroup for fetch goroutines so we can close fetchedCh
	// when all fetches are done.
	var fetchWg sync.WaitGroup

	// Offset producer
	fetchWg.Add(1)
	go func() {
		defer fetchWg.Done()
		defer close(offsetCh)
		for _, off := range p.mapping.Offsets {
			select {
			case <-p.ctx.Done():
				return
			case offsetCh <- off:
			}
		}
	}()

	// Fetch workers
	for i := 0; i < p.fetchWorkers; i++ {
		fetchWg.Add(1)
		go func() {
			defer fetchWg.Done()
			for offset := range offsetCh {
				select {
				case <-p.ctx.Done():
					return
				default:
				}
				pageData, err := p.getPageData(p.ctx, uint64(offset))
				if err != nil {
					continue // skip this page, demand-fault will handle it
				}
				select {
				case <-p.ctx.Done():
					return
				case fetchedCh <- prefetchItem{offset: offset, pageData: pageData}:
				}
			}
		}()
	}

	// Close fetchedCh when all fetch workers are done.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		fetchWg.Wait()
		close(fetchedCh)
	}()

	// Phase 2: copy workers — wait for UFFD connection, then install pages.
	for i := 0; i < p.copyWorkers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()

			// Wait for the UFFD connection to be established.
			select {
			case <-p.ctx.Done():
				return
			case <-p.connected:
			}

			for item := range fetchedCh {
				select {
				case <-p.ctx.Done():
					return
				default:
				}
				if err := p.prefault(p.uffdFd, item.offset, item.pageData); err != nil {
					p.logger.WithError(err).WithField("offset", item.offset).Debug("Prefault failed")
				}
			}
		}()
	}

	p.logger.Debug("Prefetcher fetch and copy workers started")
}

// Stop cancels all prefetcher goroutines and waits for them to finish.
func (p *Prefetcher) Stop() {
	p.cancel()
	p.wg.Wait()
}

// getPageData retrieves a single page from the chunk store using snapshot metadata.
func (p *Prefetcher) getPageData(ctx context.Context, offset uint64) ([]byte, error) {
	if p.metadata == nil {
		return zeroPage, nil
	}
	chunks := p.metadata.MemChunks
	if len(chunks) == 0 {
		return zeroPage, nil
	}

	// Binary search for chunk containing offset
	lo, hi := 0, len(chunks)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		chunk := &chunks[mid]
		if uint64(chunk.Offset) <= offset && offset < uint64(chunk.Offset+chunk.Size) {
			if chunk.IsZeroChunk() {
				return zeroPage, nil
			}
			chunkData, err := p.chunkStore.GetChunk(ctx, chunk.Hash)
			if err != nil {
				return nil, err
			}
			pageOffset := offset - uint64(chunk.Offset)
			if pageOffset+PageSize > uint64(len(chunkData)) {
				page := make([]byte, PageSize)
				copy(page, chunkData[pageOffset:])
				return page, nil
			}
			return chunkData[pageOffset : pageOffset+PageSize], nil
		}
		if uint64(chunk.Offset) > offset {
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	return zeroPage, nil
}

// prefault installs a page into guest memory via UFFDIO_COPY. Returns nil on
// success or EEXIST (page already resolved by demand fault).
func (p *Prefetcher) prefault(uffdFd int, offset int64, pageData []byte) error {
	// Find the mapping containing this offset to compute the host virtual address.
	var hostAddr uint64
	found := false
	for i := range p.mappings {
		m := &p.mappings[i]
		if uint64(offset) >= uint64(m.Offset) && uint64(offset) < uint64(m.Offset)+uint64(m.Size) {
			hostAddr = uint64(m.BaseHostVirtAddr) + uint64(offset) - uint64(m.Offset)
			found = true
			break
		}
	}
	if !found {
		return nil // offset not in any mapping, skip
	}

	cp := uffdioCopy{
		Dst:  hostAddr,
		Src:  uint64(uintptr(unsafe.Pointer(&pageData[0]))),
		Len:  PageSize,
		Mode: 0,
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffdFd), UFFDIO_COPY, uintptr(unsafe.Pointer(&cp)))
	if errno != 0 {
		if errno == unix.EEXIST {
			return nil // already resolved by demand fault
		}
		return errno
	}
	return nil
}
