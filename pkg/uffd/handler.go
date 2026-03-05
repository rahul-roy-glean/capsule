//go:build linux
// +build linux

// Package uffd implements a userfaultfd handler for lazy memory loading from chunked snapshots.
//
// When Firecracker is configured with UFFD backend for memory, page faults in the guest
// VM are forwarded to our handler via a Unix socket. The handler:
// 1. Receives page fault notifications (address, flags)
// 2. Maps the faulting address to a chunk via metadata
// 3. Fetches the chunk from cache (local or remote)
// 4. Uses UFFDIO_COPY to satisfy the fault
//
// This enables sub-second VM restore by lazily loading memory pages on demand.
package uffd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sys/unix"

	fcrotel "github.com/rahul-roy-glean/bazel-firecracker/pkg/otel"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// GuestRegionUFFDMapping represents a mapping between a VM memory address
// and the offset in the memory snapshot file. Firecracker sends these over
// the UFFD socket to tell the handler how to resolve page faults.
type GuestRegionUFFDMapping struct {
	BaseHostVirtAddr uintptr `json:"base_host_virt_addr"`
	Size             uintptr `json:"size"`
	Offset           uintptr `json:"offset"`
}

// ContainsGuestAddr returns true if addr falls within this mapping's range.
func (g *GuestRegionUFFDMapping) ContainsGuestAddr(addr uintptr) bool {
	return addr >= g.BaseHostVirtAddr && addr < g.BaseHostVirtAddr+g.Size
}

const (
	// UFFD ioctl commands
	UFFDIO_API      = 0xc018aa3f
	UFFDIO_REGISTER = 0xc020aa00
	UFFDIO_COPY     = 0xc028aa03
	UFFDIO_ZEROPAGE = 0xc020aa04
	UFFDIO_WAKE     = 0xc010aa02

	// UFFD API features
	UFFD_API          = 0xaa
	UFFD_API_FEATURES = 0

	// UFFD event types
	UFFD_EVENT_PAGEFAULT = 0x12
	UFFD_EVENT_FORK      = 0x13
	UFFD_EVENT_REMAP     = 0x14
	UFFD_EVENT_REMOVE    = 0x15
	UFFD_EVENT_UNMAP     = 0x16

	// Page fault flags
	UFFD_PAGEFAULT_FLAG_WRITE = 1 << 0
	UFFD_PAGEFAULT_FLAG_WP    = 1 << 1

	// Page size (4KB on x86_64)
	PageSize = 4096

	// uffdMsgSize is the size of a uffd_msg struct (32 bytes on x86_64).
	// Pre-computed to avoid unsafe.Sizeof in the hot loop.
	uffdMsgSize = 32
)

// zeroPage is a shared zero-filled page used for zero chunk reads.
// Safe to share because UFFDIO_COPY only reads from the source buffer.
var zeroPage = make([]byte, PageSize)

const (
	// Eager prefetching constants
	numChunksToEagerFetch = 32
)

// uffd_msg layout (32 bytes on x86_64):
//
//	Offset  Size  Field
//	0       1     event (uint8)
//	1       7     padding
//	8       8     pagefault.flags (uint64)
//	16      8     pagefault.address (uint64)
//	24      8     pagefault.feat (uint64)
//
// We parse this directly from a [uffdMsgSize]byte buffer to avoid
// struct allocation in the hot page-fault loop.

// ufffdioCopy is the UFFDIO_COPY ioctl structure
type uffdioCopy struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// Handler handles UFFD page faults by fetching memory chunks on demand
type Handler struct {
	chunkStore *snapshot.ChunkStore
	metadata   *snapshot.ChunkedSnapshotMetadata

	// Guest memory region mappings received from Firecracker.
	// These map guest virtual addresses to snapshot file offsets.
	mappings []GuestRegionUFFDMapping

	// Chunk lookup table: chunk index -> ChunkRef
	// Pre-computed for fast lookups (built after mappings are received)
	chunkLookup []snapshot.ChunkRef

	// Stats
	pageFaults   uint64
	chunkFetches uint64

	// Unix socket path for receiving UFFD from Firecracker
	socketPath string
	listener   net.Listener

	// Control
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	connected chan struct{} // closed when Firecracker connects

	// Concurrent fault handling: when faultSem is non-nil, page faults are
	// dispatched to goroutines gated by this buffered channel (semaphore).
	// When nil, faults are handled serially (legacy behavior).
	faultSem chan struct{}

	// Prefetch tracking: records page fault order for replay on next resume.
	// nil when tracking is disabled.
	prefetchTracker *PrefetchTracker

	// Prefetcher: replays recorded access patterns from a previous run.
	// Set via SetPrefetcher() before the UFFD connection is established.
	// The handler wires the UFFD fd and mappings into the prefetcher
	// when Firecracker connects.
	prefetcher *Prefetcher

	// Fault policy
	consecutiveFailures    uint64 // atomic
	killOnce               sync.Once
	onFatal                func(error)
	faultTimeout           time.Duration
	maxConsecutiveFailures int

	// OTel instruments
	faultServiceHist metric.Float64Histogram

	logger *logrus.Entry
}

// HandlerConfig holds configuration for the UFFD handler
type HandlerConfig struct {
	SocketPath string
	ChunkStore *snapshot.ChunkStore
	Metadata   *snapshot.ChunkedSnapshotMetadata
	Logger     *logrus.Logger

	// FaultTimeout is the per-fault timeout for fetching page data.
	// Zero means 5s default.
	FaultTimeout time.Duration
	// MaxConsecutiveFailures is the number of consecutive fault failures
	// before invoking OnFatal. Zero means 3 default.
	MaxConsecutiveFailures int
	// OnFatal is called (at most once) when consecutive failures reach
	// the threshold. The caller should cancel the handler's context to
	// stop the fault loop.
	OnFatal func(error)

	// Meter is an OTel metric.Meter for fault-service instrumentation.
	// If nil, no metrics are recorded.
	Meter metric.Meter

	// FaultConcurrency controls how many page faults can be serviced in
	// parallel. 0 or 1 means serial (legacy behavior). Values > 1 enable
	// concurrent fault dispatch via a bounded goroutine pool, eliminating
	// head-of-line blocking when one fault is waiting on a GCS fetch.
	// Recommended: 32.
	FaultConcurrency int

	// EnablePrefetchTracking enables recording page fault access order.
	// When true, the handler creates a PrefetchTracker that records the
	// first access to each page offset. The recorded mapping can be
	// retrieved via GetPrefetchMapping() and stored in snapshot metadata
	// for replay on subsequent resumes.
	EnablePrefetchTracking bool
}

// NewHandler creates a new UFFD handler
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	faultTimeout := cfg.FaultTimeout
	if faultTimeout == 0 {
		faultTimeout = 5 * time.Second
	}
	maxConsecutiveFailures := cfg.MaxConsecutiveFailures
	if maxConsecutiveFailures == 0 {
		maxConsecutiveFailures = 3
	}

	ctx, cancel := context.WithCancel(context.Background())

	h := &Handler{
		chunkStore:             cfg.ChunkStore,
		metadata:               cfg.Metadata,
		socketPath:             cfg.SocketPath,
		ctx:                    ctx,
		cancel:                 cancel,
		connected:              make(chan struct{}),
		onFatal:                cfg.OnFatal,
		faultTimeout:           faultTimeout,
		maxConsecutiveFailures: maxConsecutiveFailures,
		logger:                 cfg.Logger.WithField("component", "uffd-handler"),
	}

	// Enable concurrent fault handling if configured.
	if cfg.FaultConcurrency > 1 {
		h.faultSem = make(chan struct{}, cfg.FaultConcurrency)
	}

	// Enable prefetch tracking if configured.
	if cfg.EnablePrefetchTracking {
		h.prefetchTracker = NewPrefetchTracker(PageSize)
	}

	// Initialize OTel instruments if a Meter was provided.
	if cfg.Meter != nil {
		h.faultServiceHist, _ = fcrotel.NewHistogram(cfg.Meter, fcrotel.UFFDFaultServiceDuration)
	}

	// Chunk lookup is built after receiving memory mappings from Firecracker
	// in handleConnection(), not here.

	return h, nil
}

// buildChunkLookup creates a fast lookup table from memory offset to chunk
func (h *Handler) buildChunkLookup() {
	if h.metadata == nil || len(h.metadata.MemChunks) == 0 {
		return
	}

	// Chunks are sorted by offset, just copy them
	h.chunkLookup = make([]snapshot.ChunkRef, len(h.metadata.MemChunks))
	copy(h.chunkLookup, h.metadata.MemChunks)

	h.logger.WithField("chunks", len(h.chunkLookup)).Debug("Built chunk lookup table")
}

// findMapping returns the GuestRegionUFFDMapping that contains the given
// guest virtual address.
func (h *Handler) findMapping(addr uintptr) (*GuestRegionUFFDMapping, error) {
	for i := range h.mappings {
		if h.mappings[i].ContainsGuestAddr(addr) {
			return &h.mappings[i], nil
		}
	}
	return nil, fmt.Errorf("address 0x%x not found in any guest region UFFD mapping", addr)
}

// findChunk finds the chunk containing the given offset using binary search
func (h *Handler) findChunk(offset uint64) *snapshot.ChunkRef {
	chunks := h.chunkLookup
	if len(chunks) == 0 {
		return nil
	}

	// Binary search for the chunk containing this offset
	lo, hi := 0, len(chunks)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		chunk := &chunks[mid]

		if uint64(chunk.Offset) <= offset && offset < uint64(chunk.Offset+chunk.Size) {
			return chunk
		}

		if uint64(chunk.Offset) > offset {
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}

	return nil
}

// Start starts the UFFD handler, listening on the Unix socket
func (h *Handler) Start() error {
	// Remove existing socket file
	os.Remove(h.socketPath)

	listener, err := net.Listen("unix", h.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", h.socketPath, err)
	}
	h.listener = listener

	h.logger.WithField("socket", h.socketPath).Info("UFFD handler listening")

	h.wg.Add(1)
	go h.acceptLoop()

	return nil
}

// acceptLoop accepts connections from Firecracker
func (h *Handler) acceptLoop() {
	defer h.wg.Done()

	for {
		conn, err := h.listener.Accept()
		if err != nil {
			select {
			case <-h.ctx.Done():
				return
			default:
				h.logger.WithError(err).Error("Accept failed")
				continue
			}
		}

		h.logger.Info("Firecracker connected to UFFD handler")

		// Signal that a connection has been received
		select {
		case <-h.connected:
			// already closed
		default:
			close(h.connected)
		}

		// Handle this connection
		h.wg.Add(1)
		go h.handleConnection(conn)
	}
}

// handleConnection handles a single connection from Firecracker
func (h *Handler) handleConnection(conn net.Conn) {
	defer h.wg.Done()
	defer conn.Close()

	// Firecracker sends the UFFD file descriptor over this socket
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		h.logger.Error("Connection is not a Unix socket")
		return
	}

	// Receive the UFFD fd and guest memory region mappings
	uffdFd, mappings, err := h.receiveUffdAndMappings(unixConn)
	if err != nil {
		h.logger.WithError(err).Error("Failed to receive UFFD setup message")
		return
	}
	defer unix.Close(uffdFd)

	// Store mappings and build chunk lookup now that we know the address layout
	h.mappings = mappings
	h.buildChunkLookup()

	nonZeroChunks := 0
	for i := range h.metadata.MemChunks {
		if !h.metadata.MemChunks[i].IsZeroChunk() {
			nonZeroChunks++
		}
	}

	// Wire the prefetcher with the UFFD fd and mappings before signaling
	// connection (the prefetcher's copy workers wait on h.connected).
	if h.prefetcher != nil {
		h.prefetcher.SetUFFD(uffdFd, mappings)
	}

	h.logger.WithFields(logrus.Fields{
		"fd":              uffdFd,
		"mappings":        len(mappings),
		"total_chunks":    len(h.metadata.MemChunks),
		"non_zero_chunks": nonZeroChunks,
	}).Info("Received UFFD file descriptor and memory mappings")

	// Handle page faults
	h.handlePageFaults(uffdFd)
}

// receiveUffdAndMappings receives the UFFD file descriptor and guest memory
// region mappings from Firecracker via the Unix socket. Firecracker sends:
// - In-band data: JSON-encoded []GuestRegionUFFDMapping
// - Out-of-band (SCM_RIGHTS): the UFFD file descriptor
func (h *Handler) receiveUffdAndMappings(conn *net.UnixConn) (int, []GuestRegionUFFDMapping, error) {
	// Read using the higher-level UnixConn API which handles SCM_RIGHTS parsing.
	mappingsBuf := make([]byte, 4096)         // enough for JSON mappings
	oobBuf := make([]byte, unix.CmsgSpace(4)) // space for one fd

	n, oobn, _, _, err := conn.ReadMsgUnix(mappingsBuf, oobBuf)
	if err != nil {
		return -1, nil, fmt.Errorf("failed to read unix msg: %w", err)
	}

	h.logger.WithFields(logrus.Fields{
		"n":    n,
		"oobn": oobn,
	}).Debug("Received UFFD setup message")

	// Parse the guest memory region mappings from in-band data
	var mappings []GuestRegionUFFDMapping
	if n > 0 {
		if err := json.Unmarshal(mappingsBuf[:n], &mappings); err != nil {
			return -1, nil, fmt.Errorf("failed to parse memory mappings: %w", err)
		}
		h.logger.WithField("mappings", len(mappings)).Debug("Parsed guest region UFFD mappings")
		for i, m := range mappings {
			h.logger.WithFields(logrus.Fields{
				"region":    i,
				"base_addr": fmt.Sprintf("0x%x", m.BaseHostVirtAddr),
				"size":      m.Size,
				"offset":    m.Offset,
			}).Debug("Guest region mapping")
		}
	}

	// Parse the UFFD fd from out-of-band (control message) data
	uffdFd := -1
	if oobn > 0 {
		msgs, err := unix.ParseSocketControlMessage(oobBuf[:oobn])
		if err != nil {
			return -1, nil, fmt.Errorf("failed to parse control message: %w", err)
		}
		for _, msg := range msgs {
			fds, err := unix.ParseUnixRights(&msg)
			if err != nil {
				continue
			}
			if len(fds) > 0 {
				uffdFd = fds[0]
				break
			}
		}
	}

	if uffdFd < 0 {
		return -1, nil, fmt.Errorf("no UFFD file descriptor received")
	}

	if len(mappings) == 0 {
		return -1, nil, fmt.Errorf("no guest region mappings received")
	}

	return uffdFd, mappings, nil
}

// handlePageFaults reads and handles page fault events from the UFFD
func (h *Handler) handlePageFaults(uffdFd int) {
	h.logger.Info("Starting page fault handler loop")

	pollFds := []unix.PollFd{
		{Fd: int32(uffdFd), Events: unix.POLLIN},
	}

	// Pre-allocate message buffer outside the hot loop to avoid
	// per-fault heap allocation. uffd_msg is 32 bytes on x86_64.
	var msgBuf [uffdMsgSize]byte
	var lastFaultLog time.Time
	var lastFaultCount uint64

	for {
		// Periodic fault activity log (every 5s if there were faults)
		if time.Since(lastFaultLog) > 5*time.Second {
			currentFaults := atomic.LoadUint64(&h.pageFaults)
			if currentFaults != lastFaultCount {
				cacheStats := h.chunkStore.CacheStats()
				h.logger.WithFields(logrus.Fields{
					"total_faults":   currentFaults,
					"delta":          currentFaults - lastFaultCount,
					"chunk_fetches":  atomic.LoadUint64(&h.chunkFetches),
					"lru_hits":       cacheStats.Hits,
					"lru_misses":     cacheStats.Misses,
					"lru_evictions":  cacheStats.Evictions,
					"lru_items":      cacheStats.ItemCount,
					"lru_size_mb":    cacheStats.Size / (1024 * 1024),
				}).Debug("UFFD fault activity")
				lastFaultCount = currentFaults
			}
			lastFaultLog = time.Now()
		}

		select {
		case <-h.ctx.Done():
			return
		default:
		}

		// Poll with timeout to allow checking for cancellation
		n, err := unix.Poll(pollFds, 100) // 100ms timeout
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			h.logger.WithError(err).Error("Poll failed")
			return
		}

		if n == 0 {
			continue // Timeout, check for cancellation
		}

		// Read the fault message into stack-allocated buffer
		_, err = unix.Read(uffdFd, msgBuf[:])
		if err != nil {
			if err == unix.EAGAIN {
				continue
			}
			h.logger.WithError(err).Error("Failed to read UFFD message")
			return
		}

		// Parse message from stack buffer
		event := msgBuf[0]
		if event != UFFD_EVENT_PAGEFAULT {
			h.logger.WithField("event", event).Debug("Non-pagefault event")
			continue
		}

		address := binary.LittleEndian.Uint64(msgBuf[16:24])
		atomic.AddUint64(&h.pageFaults, 1)

		if h.faultSem != nil {
			// Concurrent mode: dispatch to a goroutine gated by semaphore.
			// Extract address before dispatch to avoid data race on msgBuf.
			addr := address
			h.wg.Add(1)
			h.faultSem <- struct{}{} // blocks when pool is full
			go func() {
				defer h.wg.Done()
				defer func() { <-h.faultSem }()
				if err := h.handleSingleFault(uffdFd, addr); err != nil {
					h.logger.WithError(err).WithField("address", fmt.Sprintf("0x%x", addr)).Error("Failed to handle page fault")
				}
			}()
		} else {
			// Serial mode (legacy): handle inline.
			if err := h.handleSingleFault(uffdFd, address); err != nil {
				h.logger.WithError(err).WithField("address", fmt.Sprintf("0x%x", address)).Error("Failed to handle page fault")
			}
		}
	}
}

// handleSingleFault handles a single page fault
func (h *Handler) handleSingleFault(uffdFd int, address uint64) error {
	faultStart := time.Now()

	// Align to page boundary
	pageAddr := address & ^uint64(PageSize-1)

	// Find which guest memory region contains this address
	mapping, err := h.findMapping(uintptr(pageAddr))
	if err != nil {
		return err
	}

	// Translate guest address to snapshot file offset using the mapping
	offset := uint64(uintptr(pageAddr) - mapping.BaseHostVirtAddr + mapping.Offset)

	// Queue eager fetch for upcoming chunks (async, non-blocking)
	h.queueEagerFetch(offset)

	// Fast path: use UFFDIO_ZEROPAGE for zero/missing chunks.
	// This is faster than UFFDIO_COPY because the kernel maps a shared zero
	// page without copying any data.
	chunk := h.findChunk(offset)
	if chunk == nil || chunk.IsZeroChunk() {
		return h.zeroFault(uffdFd, pageAddr)
	}

	// Get page data from chunk store with per-fault timeout
	faultCtx, cancel := context.WithTimeout(h.ctx, h.faultTimeout)
	defer cancel()

	pageData, err := h.getPageData(faultCtx, offset)
	if err != nil {
		failures := atomic.AddUint64(&h.consecutiveFailures, 1)
		h.logger.WithError(err).WithFields(logrus.Fields{
			"hash":                 chunk.Hash[:12],
			"offset":               offset,
			"consecutive_failures": failures,
		}).Error("Fault failure")
		if int(failures) >= h.maxConsecutiveFailures && h.onFatal != nil {
			h.killOnce.Do(func() { h.onFatal(err) })
		}
		return fmt.Errorf("failed to get page data: %w", err)
	}

	// Reset consecutive failures on success
	atomic.StoreUint64(&h.consecutiveFailures, 0)

	// Copy data to faulting address using UFFDIO_COPY
	cp := uffdioCopy{
		Dst:  pageAddr,
		Src:  uint64(uintptr(unsafe.Pointer(&pageData[0]))),
		Len:  PageSize,
		Mode: 0,
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffdFd), UFFDIO_COPY, uintptr(unsafe.Pointer(&cp)))
	if errno != 0 {
		// EEXIST means another goroutine already resolved this page — not an error.
		if errno == unix.EEXIST {
			return nil
		}
		return fmt.Errorf("UFFDIO_COPY failed: %w", errno)
	}

	if h.faultServiceHist != nil {
		h.faultServiceHist.Record(context.Background(), time.Since(faultStart).Seconds())
	}

	// Record access for prefetch replay on subsequent resumes.
	if h.prefetchTracker != nil {
		h.prefetchTracker.Add(int64(offset))
	}

	return nil
}

// zeroFault resolves a page fault by copying a zero-filled page.
// We use UFFDIO_COPY with a static zero buffer rather than UFFDIO_ZEROPAGE
// because UFFDIO_ZEROPAGE only works with hugetlbfs/shmem, not the anonymous
// memory mappings that Firecracker uses.
func (h *Handler) zeroFault(uffdFd int, pageAddr uint64) error {
	cp := uffdioCopy{
		Dst:  pageAddr,
		Src:  uint64(uintptr(unsafe.Pointer(&zeroPage[0]))),
		Len:  PageSize,
		Mode: 0,
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffdFd), UFFDIO_COPY, uintptr(unsafe.Pointer(&cp)))
	if errno != 0 {
		// EEXIST means another goroutine already resolved this page — not an error.
		if errno == unix.EEXIST {
			return nil
		}
		return fmt.Errorf("UFFDIO_COPY (zero page) failed: %w", errno)
	}

	return nil
}

// queueEagerFetch queues the next N chunks for background prefetching
func (h *Handler) queueEagerFetch(currentOffset uint64) {
	if h.metadata == nil {
		return
	}

	chunkSize := uint64(h.metadata.ChunkSize)
	if chunkSize == 0 {
		return
	}

	// Pre-allocate with expected capacity to avoid repeated slice growth
	hashes := make([]string, 0, numChunksToEagerFetch)
	for i := 1; i <= numChunksToEagerFetch; i++ {
		nextOffset := currentOffset + uint64(i)*chunkSize
		if chunk := h.findChunk(nextOffset); chunk != nil && !chunk.IsZeroChunk() {
			hashes = append(hashes, chunk.Hash)
		}
	}

	if len(hashes) > 0 {
		h.chunkStore.QueueEagerFetch(hashes)
	}
}

// getPageData retrieves page data by fetching the containing chunk from the
// ChunkStore (which has its own sharded LRU cache) and indexing directly into
// the chunk buffer. This avoids the 1024 separate 4KB allocations that the
// previous page-level LRU approach required per chunk fetch.
func (h *Handler) getPageData(ctx context.Context, offset uint64) ([]byte, error) {
	// Find the chunk containing this offset
	chunk := h.findChunk(offset)
	if chunk == nil || chunk.IsZeroChunk() {
		// Return shared zero page for unmapped regions or zero chunks.
		// Safe because UFFDIO_COPY only reads from the source buffer.
		return zeroPage, nil
	}

	// Fetch the chunk (hits ChunkStore's in-memory LRU on repeat access)
	atomic.AddUint64(&h.chunkFetches, 1)
	chunkData, err := h.chunkStore.GetChunk(ctx, chunk.Hash)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch chunk %s: %w", chunk.Hash[:12], err)
	}

	// Index directly into the chunk buffer for the requested page
	pageOffset := offset - uint64(chunk.Offset)
	if pageOffset+PageSize > uint64(len(chunkData)) {
		// Partial page at end of chunk - pad with zeros
		page := make([]byte, PageSize)
		copy(page, chunkData[pageOffset:])
		return page, nil
	}

	return chunkData[pageOffset : pageOffset+PageSize], nil
}

// Stop stops the UFFD handler and any associated prefetcher.
func (h *Handler) Stop() {
	h.cancel()
	if h.listener != nil {
		h.listener.Close()
	}
	h.wg.Wait()
	if h.prefetcher != nil {
		h.prefetcher.Stop()
	}
}

// GetPrefetchMapping stops the prefetch tracker and returns the recorded
// access-order mapping. Returns nil if tracking was not enabled.
func (h *Handler) GetPrefetchMapping() *snapshot.PrefetchMapping {
	if h.prefetchTracker == nil {
		return nil
	}
	return h.prefetchTracker.GetMapping()
}

// Mappings returns the guest memory region mappings received from Firecracker.
// These are available after the UFFD connection is established.
func (h *Handler) Mappings() []GuestRegionUFFDMapping {
	return h.mappings
}

// Connected returns a channel that is closed when Firecracker connects to the
// UFFD handler. Used by the Prefetcher to know when UFFDIO_COPY is available.
func (h *Handler) Connected() <-chan struct{} {
	return h.connected
}

// SetPrefetcher registers a Prefetcher to be wired up when Firecracker connects.
// Must be called before Start(). The handler will call SetUFFD on the prefetcher
// with the fd and mappings when the connection is established.
func (h *Handler) SetPrefetcher(p *Prefetcher) {
	h.prefetcher = p
}

// Stats returns handler statistics
func (h *Handler) Stats() HandlerStats {
	return HandlerStats{
		PageFaults:   atomic.LoadUint64(&h.pageFaults),
		ChunkFetches: atomic.LoadUint64(&h.chunkFetches),
		// CacheHits is always 0 for the chunked Handler: page-level caching
		// was removed in favor of the ChunkStore's chunk-level LRU, which
		// tracks its own hit rate via ChunkStore.CacheStats().
	}
}

// HandlerStats holds UFFD handler statistics
type HandlerStats struct {
	PageFaults   uint64
	CacheHits    uint64 // Always 0 for Handler; retained for API compatibility with LayeredHandler
	ChunkFetches uint64
}

// SocketPath returns the Unix socket path
func (h *Handler) SocketPath() string {
	return h.socketPath
}

// WaitForConnection waits for Firecracker to connect with a timeout
func (h *Handler) WaitForConnection(timeout time.Duration) error {
	select {
	case <-h.connected:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for Firecracker UFFD connection after %v", timeout)
	case <-h.ctx.Done():
		return h.ctx.Err()
	}
}
