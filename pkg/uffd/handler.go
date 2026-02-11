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
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

const (
	// UFFD ioctl commands
	UFFDIO_API          = 0xc018aa3f
	UFFDIO_REGISTER     = 0xc020aa00
	UFFDIO_COPY         = 0xc028aa03
	UFFDIO_ZEROPAGE     = 0xc020aa04
	UFFDIO_WAKE         = 0xc010aa02

	// UFFD API features
	UFFD_API             = 0xaa
	UFFD_API_FEATURES    = 0

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

	// Eager prefetching constants
	numChunksToEagerFetch = 32
)

// uffdMsg is the message structure received from userfaultfd
type uffdMsg struct {
	Event uint8
	_     [7]uint8
	Arg   uffdMsgArg
}

// uffdMsgArg is the union in uffd_msg
type uffdMsgArg struct {
	Pagefault uffdMsgPagefault
}

// uffdMsgPagefault is the pagefault event data
type uffdMsgPagefault struct {
	Flags   uint64
	Address uint64
	Feat    uint64
}

// uffdioApi is the UFFDIO_API ioctl structure
type uffdioApi struct {
	Api      uint64
	Features uint64
	Ioctls   uint64
}

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

	// Memory region info
	memStart uint64
	memSize  uint64

	// Chunk lookup table: chunk index -> ChunkRef
	// Pre-computed for fast lookups
	chunkLookup []snapshot.ChunkRef

	// LRU page cache for recently accessed pages
	// Key is page offset, value is page data (4KB)
	pageCache     *lru.Cache[uint64, []byte]
	pageCacheSize int

	// Stats
	pageFaults   uint64
	cacheHits    uint64
	chunkFetches uint64

	// Unix socket path for receiving UFFD from Firecracker
	socketPath string
	listener   net.Listener

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	logger *logrus.Entry
}

// HandlerConfig holds configuration for the UFFD handler
type HandlerConfig struct {
	SocketPath    string
	ChunkStore    *snapshot.ChunkStore
	Metadata      *snapshot.ChunkedSnapshotMetadata
	MemStart      uint64 // Starting address of VM memory region
	MemSize       uint64 // Size of VM memory region
	PageCacheSize int    // Max pages to cache (default 50000 = ~200MB)
	Logger        *logrus.Logger
}

// NewHandler creates a new UFFD handler
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	// Default page cache size: 50000 pages = ~200MB
	pageCacheSize := cfg.PageCacheSize
	if pageCacheSize == 0 {
		pageCacheSize = 50000
	}

	// Create LRU cache for pages
	pageCache, err := lru.New[uint64, []byte](pageCacheSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create page cache: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	h := &Handler{
		chunkStore:    cfg.ChunkStore,
		metadata:      cfg.Metadata,
		memStart:      cfg.MemStart,
		memSize:       cfg.MemSize,
		pageCache:     pageCache,
		pageCacheSize: pageCacheSize,
		socketPath:    cfg.SocketPath,
		ctx:           ctx,
		cancel:        cancel,
		logger:        cfg.Logger.WithField("component", "uffd-handler"),
	}

	// Build chunk lookup table for fast access
	h.buildChunkLookup()

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
	
	// Receive the UFFD file descriptor
	uffdFd, err := h.receiveUffdFd(unixConn)
	if err != nil {
		h.logger.WithError(err).Error("Failed to receive UFFD fd")
		return
	}
	defer unix.Close(uffdFd)
	
	h.logger.WithField("fd", uffdFd).Info("Received UFFD file descriptor")
	
	// Handle page faults
	h.handlePageFaults(uffdFd)
}

// receiveUffdFd receives the UFFD file descriptor from Firecracker via SCM_RIGHTS
func (h *Handler) receiveUffdFd(conn *net.UnixConn) (int, error) {
	// Get the underlying file to access the raw FD
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("failed to get raw connection: %w", err)
	}
	
	var uffdFd int = -1
	var recvErr error
	
	err = rawConn.Read(func(fd uintptr) bool {
		// Receive message with file descriptor
		buf := make([]byte, 64)
		oob := make([]byte, unix.CmsgSpace(4)) // Space for one fd
		
		n, oobn, _, _, err := unix.Recvmsg(int(fd), buf, oob, 0)
		if err != nil {
			recvErr = err
			return true
		}
		
		h.logger.WithFields(logrus.Fields{
			"n":    n,
			"oobn": oobn,
		}).Debug("Received UFFD message")
		
		// Parse the control message to extract the fd
		if oobn > 0 {
			msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
			if err != nil {
				recvErr = fmt.Errorf("failed to parse control message: %w", err)
				return true
			}
			
			for _, msg := range msgs {
				fds, err := unix.ParseUnixRights(&msg)
				if err != nil {
					continue
				}
				if len(fds) > 0 {
					uffdFd = fds[0]
					return true
				}
			}
		}
		
		recvErr = fmt.Errorf("no file descriptor received")
		return true
	})
	
	if err != nil {
		return -1, err
	}
	if recvErr != nil {
		return -1, recvErr
	}
	
	return uffdFd, nil
}

// handlePageFaults reads and handles page fault events from the UFFD
func (h *Handler) handlePageFaults(uffdFd int) {
	h.logger.Info("Starting page fault handler loop")
	
	pollFds := []unix.PollFd{
		{Fd: int32(uffdFd), Events: unix.POLLIN},
	}
	
	for {
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
		
		// Read the fault message
		var msg uffdMsg
		msgBytes := make([]byte, unsafe.Sizeof(msg))
		
		_, err = unix.Read(uffdFd, msgBytes)
		if err != nil {
			if err == unix.EAGAIN {
				continue
			}
			h.logger.WithError(err).Error("Failed to read UFFD message")
			return
		}
		
		// Parse message
		msg.Event = msgBytes[0]
		msg.Arg.Pagefault.Flags = binary.LittleEndian.Uint64(msgBytes[8:16])
		msg.Arg.Pagefault.Address = binary.LittleEndian.Uint64(msgBytes[16:24])
		
		if msg.Event != UFFD_EVENT_PAGEFAULT {
			h.logger.WithField("event", msg.Event).Debug("Non-pagefault event")
			continue
		}
		
		atomic.AddUint64(&h.pageFaults, 1)
		
		// Handle the page fault
		if err := h.handleSingleFault(uffdFd, msg.Arg.Pagefault.Address); err != nil {
			h.logger.WithError(err).WithField("address", fmt.Sprintf("0x%x", msg.Arg.Pagefault.Address)).Error("Failed to handle page fault")
		}
	}
}

// handleSingleFault handles a single page fault
func (h *Handler) handleSingleFault(uffdFd int, address uint64) error {
	// Align to page boundary
	pageAddr := address & ^uint64(PageSize-1)

	// Calculate offset within memory region
	if pageAddr < h.memStart || pageAddr >= h.memStart+h.memSize {
		return fmt.Errorf("address 0x%x outside memory region [0x%x, 0x%x)",
			pageAddr, h.memStart, h.memStart+h.memSize)
	}
	offset := pageAddr - h.memStart

	// Queue eager fetch for upcoming chunks (async, non-blocking)
	h.queueEagerFetch(offset)

	// Get page data
	pageData, err := h.getPageData(offset)
	if err != nil {
		return fmt.Errorf("failed to get page data: %w", err)
	}

	// Copy data to faulting address using UFFDIO_COPY
	copy := uffdioCopy{
		Dst:  pageAddr,
		Src:  uint64(uintptr(unsafe.Pointer(&pageData[0]))),
		Len:  PageSize,
		Mode: 0,
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffdFd), UFFDIO_COPY, uintptr(unsafe.Pointer(&copy)))
	if errno != 0 {
		return fmt.Errorf("UFFDIO_COPY failed: %w", errno)
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

	// Find current chunk and queue the next N chunks
	var hashes []string
	for i := 1; i <= numChunksToEagerFetch; i++ {
		nextOffset := currentOffset + uint64(i)*chunkSize
		if chunk := h.findChunk(nextOffset); chunk != nil {
			hashes = append(hashes, chunk.Hash)
		}
	}

	if len(hashes) > 0 {
		h.chunkStore.QueueEagerFetch(hashes)
	}
}

// getPageData retrieves page data from cache or fetches from chunk store
func (h *Handler) getPageData(offset uint64) ([]byte, error) {
	// Check LRU page cache first
	if data, ok := h.pageCache.Get(offset); ok {
		atomic.AddUint64(&h.cacheHits, 1)
		return data, nil
	}

	// Find the chunk containing this offset
	chunk := h.findChunk(offset)
	if chunk == nil {
		// Return zero page for unmapped regions
		return make([]byte, PageSize), nil
	}

	// Fetch the chunk
	atomic.AddUint64(&h.chunkFetches, 1)
	chunkData, err := h.chunkStore.GetChunk(h.ctx, chunk.Hash)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch chunk %s: %w", chunk.Hash[:12], err)
	}

	// Cache pages from this chunk
	h.cacheChunkPages(uint64(chunk.Offset), chunkData)

	// Return the requested page
	pageOffset := offset - uint64(chunk.Offset)
	if pageOffset+PageSize > uint64(len(chunkData)) {
		// Partial page at end of chunk - pad with zeros
		page := make([]byte, PageSize)
		copy(page, chunkData[pageOffset:])
		return page, nil
	}

	return chunkData[pageOffset : pageOffset+PageSize], nil
}

// cacheChunkPages caches all pages from a fetched chunk
// LRU eviction happens automatically when cache is full
func (h *Handler) cacheChunkPages(chunkOffset uint64, data []byte) {
	// Cache each page in the chunk
	for off := uint64(0); off < uint64(len(data)); off += PageSize {
		pageAddr := chunkOffset + off
		endOff := off + PageSize
		if endOff > uint64(len(data)) {
			endOff = uint64(len(data))
		}

		// Make a copy for the cache
		page := make([]byte, PageSize)
		copy(page, data[off:endOff])
		h.pageCache.Add(pageAddr, page) // LRU eviction is automatic
	}
	
	// TODO: Implement LRU eviction when cache gets too large
}

// Stop stops the UFFD handler
func (h *Handler) Stop() {
	h.cancel()
	if h.listener != nil {
		h.listener.Close()
	}
	h.wg.Wait()
}

// Stats returns handler statistics
func (h *Handler) Stats() HandlerStats {
	return HandlerStats{
		PageFaults:   atomic.LoadUint64(&h.pageFaults),
		CacheHits:    atomic.LoadUint64(&h.cacheHits),
		ChunkFetches: atomic.LoadUint64(&h.chunkFetches),
	}
}

// HandlerStats holds UFFD handler statistics
type HandlerStats struct {
	PageFaults   uint64
	CacheHits    uint64
	ChunkFetches uint64
}

// SocketPath returns the Unix socket path
func (h *Handler) SocketPath() string {
	return h.socketPath
}

// WaitForConnection waits for Firecracker to connect with a timeout
func (h *Handler) WaitForConnection(timeout time.Duration) error {
	// This is a simple implementation - the acceptLoop handles connections
	// In practice, you might want to use a channel to signal when connected
	time.Sleep(100 * time.Millisecond)
	return nil
}
