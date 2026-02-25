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
}

// LayeredHandler handles UFFD page faults by checking diff layers then golden base.
// It is used for session resume where memory is a stack of diff snapshots.
type LayeredHandler struct {
	goldenMem  []byte  // mmap'd golden memory (read-only, shared)
	goldenSize int64   // size of golden memory file
	layerFDs   []int   // file descriptors for diff layers (oldest first)
	layerSizes []int64 // sizes of diff layer files

	// Guest memory region mappings received from Firecracker
	mappings []GuestRegionUFFDMapping

	// LRU page cache
	pageCache     *lru.Cache[uint64, []byte]
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

	// Open diff layer files (oldest first)
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
	}

	h.logger.WithFields(logrus.Fields{
		"golden_size": h.goldenSize,
		"layers":      len(h.layerFDs),
	}).Info("Layered UFFD handler initialized")

	return h, nil
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

		msg.Event = msgBytes[0]
		msg.Arg.Pagefault.Flags = binary.LittleEndian.Uint64(msgBytes[8:16])
		msg.Arg.Pagefault.Address = binary.LittleEndian.Uint64(msgBytes[16:24])

		if msg.Event != UFFD_EVENT_PAGEFAULT {
			continue
		}

		atomic.AddUint64(&h.pageFaults, 1)

		if err := h.handleSingleFault(uffdFd, msg.Arg.Pagefault.Address); err != nil {
			h.logger.WithError(err).WithField("address", fmt.Sprintf("0x%x", msg.Arg.Pagefault.Address)).Error("Failed to handle page fault")
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

	// Check page cache first
	if data, ok := h.pageCache.Get(offset); ok {
		atomic.AddUint64(&h.cacheHits, 1)
		return h.copyPage(uffdFd, pageAddr, data)
	}

	// Check diff layers in reverse order (newest first)
	for i := len(h.layerFDs) - 1; i >= 0; i-- {
		fd := h.layerFDs[i]
		layerSize := h.layerSizes[i]

		if int64(offset) >= layerSize {
			continue
		}

		// Use lseek SEEK_DATA to check if this offset has data (not a sparse hole)
		dataOffset, err := unix.Seek(fd, int64(offset), unix.SEEK_DATA)
		if err != nil {
			// ENXIO means no data at or after offset (all holes) - try next layer
			continue
		}

		// Check if the data region starts at or before our page offset
		// and we're within a data region (not past it into a hole)
		if dataOffset <= int64(offset) {
			// Read the page from this layer
			page := make([]byte, PageSize)
			n, err := unix.Pread(fd, page, int64(offset))
			if err != nil || n == 0 {
				continue
			}

			atomic.AddUint64(&h.layerReads, 1)
			h.pageCache.Add(offset, page)
			return h.copyPage(uffdFd, pageAddr, page)
		}
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
