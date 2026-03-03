//go:build linux
// +build linux

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

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// LayeredHandlerConfig holds configuration for the layered UFFD handler
type LayeredHandlerConfig struct {
	SocketPath    string   // Unix socket for Firecracker UFFD connection
	GoldenMemPath string   // Path to golden snapshot.mem (shared, read-only)
	DiffLayers    []string // Paths to diff layer files, oldest first
	PageCacheSize int      // Max pages to cache (default 50000 = ~200MB)
	Logger        *logrus.Logger

	// FaultConcurrency controls how many page faults can be serviced in
	// parallel. 0 or 1 means serial (legacy behavior). Values > 1 enable
	// concurrent fault dispatch. Recommended: 32.
	FaultConcurrency int
}

// LayeredHandler handles UFFD page faults by checking diff layers then golden base.
// It is used for session resume where memory is a stack of diff snapshots.
type LayeredHandler struct {
	goldenMem  []byte  // mmap'd golden memory (read-only, shared)
	goldenSize int64   // size of golden memory file
	layerFDs   []int   // file descriptors for diff layers (oldest first)
	layerSizes []int64 // sizes of diff layer files

	// layerBitmaps holds one bitmap per diff layer (same order as layerFDs).
	// Each bitmap has one bit per page: bit N is set if page N contains data
	// (not a sparse hole). Built at startup via SEEK_DATA/SEEK_HOLE to
	// replace per-fault lseek syscalls with O(1) bitmap lookups.
	layerBitmaps [][]uint64

	// Guest memory region mappings received from Firecracker
	mappings []GuestRegionUFFDMapping

	// LRU page cache (not goroutine-safe, guarded by pageCacheMu)
	pageCache     *lru.Cache[uint64, []byte]
	pageCacheMu   sync.RWMutex
	pageCacheSize int

	// Stats
	pageFaults  uint64
	cacheHits   uint64
	goldenReads uint64
	layerReads  uint64

	// Unix socket for Firecracker connection
	socketPath string
	listener   net.Listener

	// Control
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	connected chan struct{}

	// Concurrent fault handling semaphore (nil = serial)
	faultSem chan struct{}

	logger *logrus.Entry
}

// NewLayeredHandler creates a new layered UFFD handler
func NewLayeredHandler(cfg LayeredHandlerConfig) (*LayeredHandler, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	pageCacheSize := cfg.PageCacheSize
	if pageCacheSize == 0 {
		pageCacheSize = 50000
	}

	pageCache, err := lru.New[uint64, []byte](pageCacheSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create page cache: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	h := &LayeredHandler{
		pageCacheSize: pageCacheSize,
		pageCache:     pageCache,
		socketPath:    cfg.SocketPath,
		ctx:           ctx,
		cancel:        cancel,
		connected:     make(chan struct{}),
		logger:        cfg.Logger.WithField("component", "uffd-layered-handler"),
	}

	// Enable concurrent fault handling if configured.
	if cfg.FaultConcurrency > 1 {
		h.faultSem = make(chan struct{}, cfg.FaultConcurrency)
	}

	// mmap golden memory file (read-only, shared across all sessions)
	goldenFile, err := os.Open(cfg.GoldenMemPath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to open golden memory file: %w", err)
	}
	defer goldenFile.Close()

	goldenInfo, err := goldenFile.Stat()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to stat golden memory file: %w", err)
	}
	h.goldenSize = goldenInfo.Size()

	goldenMem, err := unix.Mmap(int(goldenFile.Fd()), 0, int(h.goldenSize),
		unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to mmap golden memory: %w", err)
	}
	h.goldenMem = goldenMem

	// Open diff layer files (oldest first) and build extent bitmaps
	for _, layerPath := range cfg.DiffLayers {
		fd, err := unix.Open(layerPath, unix.O_RDONLY, 0)
		if err != nil {
			h.cleanup()
			return nil, fmt.Errorf("failed to open diff layer %s: %w", layerPath, err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			unix.Close(fd)
			h.cleanup()
			return nil, fmt.Errorf("failed to stat diff layer %s: %w", layerPath, err)
		}
		h.layerFDs = append(h.layerFDs, fd)
		h.layerSizes = append(h.layerSizes, stat.Size)

		// Build a page-level bitmap of data extents using SEEK_DATA/SEEK_HOLE.
		// This replaces per-fault lseek syscalls with O(1) bitmap lookups.
		bitmap := buildExtentBitmap(fd, stat.Size)
		h.layerBitmaps = append(h.layerBitmaps, bitmap)
	}

	h.logger.WithFields(logrus.Fields{
		"golden_size": h.goldenSize,
		"layers":      len(h.layerFDs),
	}).Info("Layered UFFD handler initialized")

	return h, nil
}

// buildExtentBitmap walks a sparse file with SEEK_DATA/SEEK_HOLE and returns
// a bitmap with one bit per page. Bit N is set if page N has data.
func buildExtentBitmap(fd int, fileSize int64) []uint64 {
	numPages := (fileSize + PageSize - 1) / PageSize
	numWords := (numPages + 63) / 64
	bitmap := make([]uint64, numWords)

	for offset := int64(0); ; {
		dataStart, err := unix.Seek(fd, offset, unix.SEEK_DATA)
		if err != nil {
			break // ENXIO = no more data segments
		}
		holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			holeStart = fileSize
		}

		// Set bits for all pages in [dataStart, holeStart)
		firstPage := dataStart / PageSize
		lastPage := (holeStart - 1) / PageSize
		for p := firstPage; p <= lastPage; p++ {
			bitmap[p/64] |= 1 << (p % 64)
		}

		offset = holeStart
	}

	return bitmap
}

// bitmapHasPage returns true if the bitmap indicates page at the given offset
// has data (is not a sparse hole).
func bitmapHasPage(bitmap []uint64, offset uint64) bool {
	page := offset / PageSize
	word := page / 64
	if word >= uint64(len(bitmap)) {
		return false
	}
	return bitmap[word]&(1<<(page%64)) != 0
}

// cleanup releases resources
func (h *LayeredHandler) cleanup() {
	if h.goldenMem != nil {
		unix.Munmap(h.goldenMem)
		h.goldenMem = nil
	}
	for _, fd := range h.layerFDs {
		unix.Close(fd)
	}
	h.layerFDs = nil
}

// Start starts the layered UFFD handler
func (h *LayeredHandler) Start() error {
	os.Remove(h.socketPath)

	listener, err := net.Listen("unix", h.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", h.socketPath, err)
	}
	h.listener = listener

	h.logger.WithField("socket", h.socketPath).Info("Layered UFFD handler listening")

	h.wg.Add(1)
	go h.acceptLoop()

	return nil
}

// acceptLoop accepts connections from Firecracker
func (h *LayeredHandler) acceptLoop() {
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

		h.logger.Info("Firecracker connected to layered UFFD handler")

		select {
		case <-h.connected:
		default:
			close(h.connected)
		}

		h.wg.Add(1)
		go h.handleConnection(conn)
	}
}

// handleConnection handles a single connection from Firecracker
func (h *LayeredHandler) handleConnection(conn net.Conn) {
	defer h.wg.Done()
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		h.logger.Error("Connection is not a Unix socket")
		return
	}

	uffdFd, mappings, err := h.receiveUffdAndMappings(unixConn)
	if err != nil {
		h.logger.WithError(err).Error("Failed to receive UFFD setup message")
		return
	}
	defer unix.Close(uffdFd)

	h.mappings = mappings

	h.logger.WithFields(logrus.Fields{
		"fd":       uffdFd,
		"mappings": len(mappings),
	}).Info("Received UFFD file descriptor and memory mappings")

	h.handlePageFaults(uffdFd)
}

// receiveUffdAndMappings receives the UFFD fd and mappings from Firecracker
func (h *LayeredHandler) receiveUffdAndMappings(conn *net.UnixConn) (int, []GuestRegionUFFDMapping, error) {
	mappingsBuf := make([]byte, 4096)
	oobBuf := make([]byte, unix.CmsgSpace(4))

	n, oobn, _, _, err := conn.ReadMsgUnix(mappingsBuf, oobBuf)
	if err != nil {
		return -1, nil, fmt.Errorf("failed to read unix msg: %w", err)
	}

	var mappings []GuestRegionUFFDMapping
	if n > 0 {
		if err := json.Unmarshal(mappingsBuf[:n], &mappings); err != nil {
			return -1, nil, fmt.Errorf("failed to parse memory mappings: %w", err)
		}
	}

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

// findMapping returns the mapping containing the given guest virtual address
func (h *LayeredHandler) findMapping(addr uintptr) (*GuestRegionUFFDMapping, error) {
	for i := range h.mappings {
		if h.mappings[i].ContainsGuestAddr(addr) {
			return &h.mappings[i], nil
		}
	}
	return nil, fmt.Errorf("address 0x%x not found in any guest region mapping", addr)
}

// handlePageFaults reads and handles page fault events from the UFFD
func (h *LayeredHandler) handlePageFaults(uffdFd int) {
	h.logger.Info("Starting layered page fault handler loop")

	pollFds := []unix.PollFd{
		{Fd: int32(uffdFd), Events: unix.POLLIN},
	}

	// Pre-allocate message buffer outside the hot loop to avoid
	// per-fault heap allocation. uffd_msg is 32 bytes on x86_64.
	var msgBuf [uffdMsgSize]byte

	for {
		select {
		case <-h.ctx.Done():
			return
		default:
		}

		n, err := unix.Poll(pollFds, 100)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			h.logger.WithError(err).Error("Poll failed")
			return
		}

		if n == 0 {
			continue
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
			continue
		}

		address := binary.LittleEndian.Uint64(msgBuf[16:24])
		atomic.AddUint64(&h.pageFaults, 1)

		if h.faultSem != nil {
			// Concurrent mode: dispatch to a goroutine gated by semaphore.
			addr := address
			h.wg.Add(1)
			h.faultSem <- struct{}{}
			go func() {
				defer h.wg.Done()
				defer func() { <-h.faultSem }()
				if err := h.handleSingleFault(uffdFd, addr); err != nil {
					h.logger.WithError(err).WithField("address", fmt.Sprintf("0x%x", addr)).Error("Failed to handle page fault")
				}
			}()
		} else {
			if err := h.handleSingleFault(uffdFd, address); err != nil {
				h.logger.WithError(err).WithField("address", fmt.Sprintf("0x%x", address)).Error("Failed to handle page fault")
			}
		}
	}
}

// handleSingleFault resolves a page fault by checking diff layers (newest first),
// then falling back to the golden base memory.
func (h *LayeredHandler) handleSingleFault(uffdFd int, address uint64) error {
	pageAddr := address & ^uint64(PageSize-1)

	mapping, err := h.findMapping(uintptr(pageAddr))
	if err != nil {
		return err
	}

	offset := uint64(uintptr(pageAddr) - mapping.BaseHostVirtAddr + mapping.Offset)

	// Check page cache first (guarded by RWMutex for concurrent access)
	h.pageCacheMu.RLock()
	data, ok := h.pageCache.Get(offset)
	h.pageCacheMu.RUnlock()
	if ok {
		atomic.AddUint64(&h.cacheHits, 1)
		return h.copyPage(uffdFd, pageAddr, data)
	}

	// Check diff layers in reverse order (newest first)
	// Uses pre-built bitmaps for O(1) data-presence checks instead of
	// per-fault SEEK_DATA syscalls.
	for i := len(h.layerFDs) - 1; i >= 0; i-- {
		if int64(offset) >= h.layerSizes[i] {
			continue
		}

		// O(1) bitmap lookup replaces lseek(SEEK_DATA) syscall
		if !bitmapHasPage(h.layerBitmaps[i], offset) {
			continue
		}

		// Read the page from this layer
		page := make([]byte, PageSize)
		n, err := unix.Pread(h.layerFDs[i], page, int64(offset))
		if err != nil || n == 0 {
			continue
		}

		atomic.AddUint64(&h.layerReads, 1)
		h.pageCacheMu.Lock()
		h.pageCache.Add(offset, page)
		h.pageCacheMu.Unlock()
		return h.copyPage(uffdFd, pageAddr, page)
	}

	// Fall back to golden base memory
	if int64(offset)+PageSize <= h.goldenSize {
		page := h.goldenMem[offset : offset+PageSize]
		atomic.AddUint64(&h.goldenReads, 1)
		// Cache a copy (golden mem is shared read-only, UFFDIO_COPY only reads)
		return h.copyPage(uffdFd, pageAddr, page)
	}

	// Beyond golden memory - zero fill
	return h.copyPage(uffdFd, pageAddr, zeroPage)
}

// copyPage uses UFFDIO_COPY to satisfy a page fault
func (h *LayeredHandler) copyPage(uffdFd int, pageAddr uint64, data []byte) error {
	cp := uffdioCopy{
		Dst:  pageAddr,
		Src:  uint64(uintptr(unsafe.Pointer(&data[0]))),
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
	return nil
}

// Stop stops the layered UFFD handler and releases all resources
func (h *LayeredHandler) Stop() {
	h.cancel()
	if h.listener != nil {
		h.listener.Close()
	}
	h.wg.Wait()
	h.cleanup()
}

// SocketPath returns the Unix socket path
func (h *LayeredHandler) SocketPath() string {
	return h.socketPath
}

// WaitForConnection waits for Firecracker to connect with a timeout
func (h *LayeredHandler) WaitForConnection(timeout time.Duration) error {
	select {
	case <-h.connected:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for Firecracker UFFD connection after %v", timeout)
	case <-h.ctx.Done():
		return h.ctx.Err()
	}
}

// Stats returns handler statistics
func (h *LayeredHandler) Stats() LayeredHandlerStats {
	return LayeredHandlerStats{
		PageFaults:  atomic.LoadUint64(&h.pageFaults),
		CacheHits:   atomic.LoadUint64(&h.cacheHits),
		GoldenReads: atomic.LoadUint64(&h.goldenReads),
		LayerReads:  atomic.LoadUint64(&h.layerReads),
	}
}

// LayeredHandlerStats holds layered UFFD handler statistics
type LayeredHandlerStats struct {
	PageFaults  uint64
	CacheHits   uint64
	GoldenReads uint64
	LayerReads  uint64
}
