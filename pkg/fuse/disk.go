//go:build linux
// +build linux

// Package fuse implements a FUSE-based virtual disk backed by chunked snapshots.
//
// This allows Firecracker to use a "disk image" that is actually served lazily from
// content-addressed chunks stored in a remote cache. Key features:
//
// - Lazy loading: Only fetch chunks when they're actually read
// - Copy-on-write: Writes go to local dirty chunks, preserving the original
// - Deduplication: Multiple VMs can share the same base chunks
// - Network scaling: Chunks can be fetched from any cache server
//
// The FUSE filesystem presents a regular file that Firecracker can use as a block device.
package fuse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

const (
	// Eager prefetching constants
	numChunksToEagerFetch = 32
)

// ChunkedDisk implements a FUSE filesystem backed by chunked snapshots
type ChunkedDisk struct {
	chunkStore *snapshot.ChunkStore
	chunks     []snapshot.ChunkRef
	totalSize  int64
	chunkSize  int64

	// Copy-on-write dirty chunks
	// Key is chunk index, value is the modified chunk data
	dirtyChunks   map[int][]byte
	dirtyChunksMu sync.RWMutex

	// Mount configuration
	mountPoint string
	conn       *fuse.Conn

	// Stats
	reads       uint64
	writes      uint64
	chunkReads  uint64
	dirtyWrites uint64

	logger *logrus.Entry
	ctx    context.Context
	cancel context.CancelFunc
}

// ChunkedDiskConfig holds configuration for the chunked disk
type ChunkedDiskConfig struct {
	ChunkStore *snapshot.ChunkStore
	Chunks     []snapshot.ChunkRef
	TotalSize  int64
	ChunkSize  int64
	MountPoint string
	Logger     *logrus.Logger
}

// NewChunkedDisk creates a new chunked disk
func NewChunkedDisk(cfg ChunkedDiskConfig) (*ChunkedDisk, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &ChunkedDisk{
		chunkStore:  cfg.ChunkStore,
		chunks:      cfg.Chunks,
		totalSize:   cfg.TotalSize,
		chunkSize:   cfg.ChunkSize,
		dirtyChunks: make(map[int][]byte),
		mountPoint:  cfg.MountPoint,
		logger:      cfg.Logger.WithField("component", "chunked-disk"),
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// Mount mounts the FUSE filesystem
func (d *ChunkedDisk) Mount() error {
	// Ensure mount point directory exists
	if err := os.MkdirAll(d.mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Mount FUSE
	c, err := fuse.Mount(
		d.mountPoint,
		fuse.FSName("chunked-disk"),
		fuse.Subtype("firecracker"),
		fuse.AllowOther(),
		fuse.MaxReadahead(1024*1024), // 1MB readahead
	)
	if err != nil {
		return fmt.Errorf("failed to mount FUSE: %w", err)
	}
	d.conn = c

	d.logger.WithField("mount_point", d.mountPoint).Info("Chunked disk mounted")

	// Serve the filesystem in a goroutine
	go func() {
		if err := fs.Serve(c, d); err != nil {
			d.logger.WithError(err).Error("FUSE serve failed")
		}
	}()

	// Mount is ready once fuse.Mount returns successfully
	// (Ready channel and MountError were removed in newer bazil.org/fuse versions)

	return nil
}

// Unmount unmounts the FUSE filesystem
func (d *ChunkedDisk) Unmount() error {
	d.cancel()
	if d.conn != nil {
		fuse.Unmount(d.mountPoint)
		d.conn.Close()
	}
	return nil
}

// DiskImagePath returns the path to the virtual disk image file
func (d *ChunkedDisk) DiskImagePath() string {
	return filepath.Join(d.mountPoint, "disk.img")
}

// Root implements fs.FS
func (d *ChunkedDisk) Root() (fs.Node, error) {
	return &diskDir{disk: d}, nil
}

// diskDir represents the root directory containing the disk image
type diskDir struct {
	disk *ChunkedDisk
}

// Attr implements fs.Node
func (dir *diskDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0755
	return nil
}

// Lookup implements fs.NodeStringLookuper
func (dir *diskDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name == "disk.img" {
		return &diskFile{disk: dir.disk}, nil
	}
	return nil, fuse.ENOENT
}

// ReadDirAll implements fs.HandleReadDirAller
func (dir *diskDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Name: "disk.img", Type: fuse.DT_File},
	}, nil
}

// diskFile represents the virtual disk image file
type diskFile struct {
	disk *ChunkedDisk
}

// Attr implements fs.Node
func (f *diskFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0644
	a.Size = uint64(f.disk.totalSize)
	return nil
}

// Read implements fs.HandleReader
func (f *diskFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	atomic.AddUint64(&f.disk.reads, 1)

	offset := req.Offset
	size := req.Size

	// Clamp to file size
	if offset >= f.disk.totalSize {
		return nil
	}
	if offset+int64(size) > f.disk.totalSize {
		size = int(f.disk.totalSize - offset)
	}

	data := make([]byte, size)
	n, err := f.disk.ReadAt(data, offset)
	if err != nil {
		return err
	}

	resp.Data = data[:n]
	return nil
}

// Write implements fs.HandleWriter
func (f *diskFile) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	atomic.AddUint64(&f.disk.writes, 1)

	n, err := f.disk.WriteAt(req.Data, req.Offset)
	if err != nil {
		return err
	}

	resp.Size = n
	return nil
}

// Fsync implements fs.NodeFsyncer
func (f *diskFile) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	// Dirty chunks are in memory, fsync is a no-op for now
	// In production, you might persist dirty chunks to local storage
	return nil
}

// ReadAt reads data from the virtual disk at the given offset
func (d *ChunkedDisk) ReadAt(p []byte, off int64) (int, error) {
	if off >= d.totalSize {
		return 0, nil
	}

	// Queue eager fetch for upcoming chunks (async, non-blocking)
	d.queueEagerFetch(off)

	toRead := len(p)
	if off+int64(toRead) > d.totalSize {
		toRead = int(d.totalSize - off)
	}

	bytesRead := 0
	for bytesRead < toRead {
		// Find chunk containing current offset
		currentOff := off + int64(bytesRead)
		chunkIdx := int(currentOff / d.chunkSize)
		chunkOff := currentOff % d.chunkSize

		// Calculate how much to read from this chunk
		remaining := toRead - bytesRead
		chunkRemaining := int(d.chunkSize - chunkOff)
		if remaining > chunkRemaining {
			remaining = chunkRemaining
		}

		// Get chunk data (from dirty cache or fetch from store)
		chunkData, err := d.getChunkData(chunkIdx)
		if err != nil {
			return bytesRead, err
		}

		// Copy data from chunk
		n := copy(p[bytesRead:bytesRead+remaining], chunkData[chunkOff:])
		bytesRead += n
	}

	return bytesRead, nil
}

// queueEagerFetch queues the next N chunks for background prefetching
func (d *ChunkedDisk) queueEagerFetch(offset int64) {
	if d.chunkSize == 0 {
		return
	}

	startChunk := int(offset / d.chunkSize)
	var hashes []string

	for i := 1; i <= numChunksToEagerFetch; i++ {
		idx := startChunk + i
		if idx < len(d.chunks) && !d.chunks[idx].IsZeroChunk() {
			hashes = append(hashes, d.chunks[idx].Hash)
		}
	}

	if len(hashes) > 0 {
		d.chunkStore.QueueEagerFetch(hashes)
	}
}

// WriteAt writes data to the virtual disk at the given offset (copy-on-write)
func (d *ChunkedDisk) WriteAt(p []byte, off int64) (int, error) {
	if off >= d.totalSize {
		return 0, nil
	}

	toWrite := len(p)
	if off+int64(toWrite) > d.totalSize {
		toWrite = int(d.totalSize - off)
	}

	bytesWritten := 0
	for bytesWritten < toWrite {
		// Find chunk containing current offset
		currentOff := off + int64(bytesWritten)
		chunkIdx := int(currentOff / d.chunkSize)
		chunkOff := currentOff % d.chunkSize

		// Calculate how much to write to this chunk
		remaining := toWrite - bytesWritten
		chunkRemaining := int(d.chunkSize - chunkOff)
		if remaining > chunkRemaining {
			remaining = chunkRemaining
		}

		// Get or create dirty chunk
		dirtyChunk, err := d.getOrCreateDirtyChunk(chunkIdx)
		if err != nil {
			return bytesWritten, err
		}

		// Write to dirty chunk
		n := copy(dirtyChunk[chunkOff:], p[bytesWritten:bytesWritten+remaining])
		bytesWritten += n
		atomic.AddUint64(&d.dirtyWrites, 1)
	}

	return bytesWritten, nil
}

// getChunkData retrieves chunk data, checking dirty cache first
func (d *ChunkedDisk) getChunkData(chunkIdx int) ([]byte, error) {
	// Check dirty chunks first
	d.dirtyChunksMu.RLock()
	if dirty, ok := d.dirtyChunks[chunkIdx]; ok {
		d.dirtyChunksMu.RUnlock()
		return dirty, nil
	}
	d.dirtyChunksMu.RUnlock()

	// Fetch from chunk store
	if chunkIdx >= len(d.chunks) {
		// Beyond stored chunks, return zeros
		return make([]byte, d.chunkSize), nil
	}

	chunk := &d.chunks[chunkIdx]

	// Zero chunk (skipped during build) — return zeros without a network fetch
	if chunk.IsZeroChunk() {
		return make([]byte, chunk.Size), nil
	}

	atomic.AddUint64(&d.chunkReads, 1)
	return d.chunkStore.GetChunk(d.ctx, chunk.Hash)
}

// getOrCreateDirtyChunk gets an existing dirty chunk or creates one (copy-on-write)
// Uses double-checked locking to avoid holding lock during chunk fetch
func (d *ChunkedDisk) getOrCreateDirtyChunk(chunkIdx int) ([]byte, error) {
	// First check without write lock
	d.dirtyChunksMu.RLock()
	if dirty, ok := d.dirtyChunks[chunkIdx]; ok {
		d.dirtyChunksMu.RUnlock()
		return dirty, nil
	}
	d.dirtyChunksMu.RUnlock()

	// Fetch original data BEFORE acquiring write lock to avoid blocking concurrent reads
	var originalData []byte
	if chunkIdx < len(d.chunks) {
		chunk := &d.chunks[chunkIdx]
		if chunk.IsZeroChunk() {
			originalData = make([]byte, d.chunkSize)
		} else {
			var err error
			originalData, err = d.chunkStore.GetChunk(d.ctx, chunk.Hash)
			if err != nil {
				return nil, err
			}
		}
	} else {
		// Beyond stored chunks, start with zeros
		originalData = make([]byte, d.chunkSize)
	}

	// Now acquire write lock
	d.dirtyChunksMu.Lock()
	defer d.dirtyChunksMu.Unlock()

	// Double-check: another goroutine may have created the dirty chunk while we were fetching
	if dirty, ok := d.dirtyChunks[chunkIdx]; ok {
		return dirty, nil
	}

	// Make a copy for the dirty chunk
	dirty := make([]byte, d.chunkSize)
	copy(dirty, originalData)
	d.dirtyChunks[chunkIdx] = dirty

	d.logger.WithField("chunk", chunkIdx).Debug("Created dirty chunk (copy-on-write)")

	return dirty, nil
}

// GetDirtyChunks returns all dirty chunks for incremental snapshot upload
func (d *ChunkedDisk) GetDirtyChunks() map[int][]byte {
	d.dirtyChunksMu.RLock()
	defer d.dirtyChunksMu.RUnlock()

	// Return a copy to avoid concurrent modification issues
	result := make(map[int][]byte, len(d.dirtyChunks))
	for idx, data := range d.dirtyChunks {
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		result[idx] = dataCopy
	}

	return result
}

// DirtyChunkCount returns the number of dirty chunks
func (d *ChunkedDisk) DirtyChunkCount() int {
	d.dirtyChunksMu.RLock()
	defer d.dirtyChunksMu.RUnlock()
	return len(d.dirtyChunks)
}

// Stats returns disk statistics
func (d *ChunkedDisk) Stats() DiskStats {
	return DiskStats{
		Reads:       atomic.LoadUint64(&d.reads),
		Writes:      atomic.LoadUint64(&d.writes),
		ChunkReads:  atomic.LoadUint64(&d.chunkReads),
		DirtyWrites: atomic.LoadUint64(&d.dirtyWrites),
		DirtyChunks: d.DirtyChunkCount(),
	}
}

// DiskStats holds disk statistics
type DiskStats struct {
	Reads       uint64
	Writes      uint64
	ChunkReads  uint64
	DirtyWrites uint64
	DirtyChunks int
}

// SaveDirtyChunks uploads dirty chunks to the chunk store and returns updated chunk refs
func (d *ChunkedDisk) SaveDirtyChunks(ctx context.Context) ([]snapshot.ChunkRef, error) {
	d.dirtyChunksMu.RLock()
	defer d.dirtyChunksMu.RUnlock()

	// Start with a copy of original chunks
	newChunks := make([]snapshot.ChunkRef, len(d.chunks))
	copy(newChunks, d.chunks)

	// Extend if we have dirty chunks beyond the original
	for idx := range d.dirtyChunks {
		if idx >= len(newChunks) {
			// Extend the slice
			for i := len(newChunks); i <= idx; i++ {
				newChunks = append(newChunks, snapshot.ChunkRef{
					Offset: int64(i) * d.chunkSize,
					Size:   d.chunkSize,
				})
			}
		}
	}

	// Upload dirty chunks and update refs
	for idx, data := range d.dirtyChunks {
		hash, compressedSize, err := d.chunkStore.StoreChunk(ctx, data)
		if err != nil {
			return nil, fmt.Errorf("failed to store dirty chunk %d: %w", idx, err)
		}

		newChunks[idx].Hash = hash
		newChunks[idx].CompressedSize = compressedSize

		d.logger.WithFields(logrus.Fields{
			"chunk_idx": idx,
			"hash":      hash[:12],
		}).Debug("Uploaded dirty chunk")
	}

	return newChunks, nil
}
