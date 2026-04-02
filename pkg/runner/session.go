package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/rahul-roy-glean/capsule/pkg/accessplane"
	"github.com/rahul-roy-glean/capsule/pkg/firecracker"
	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
	"github.com/rahul-roy-glean/capsule/pkg/uffd"
)

const (
	// defaultSessionDir is the fallback session directory when not configured.
	defaultSessionDir = "/tmp/fc-dev/sessions"
)

// sessionBaseDir returns the session storage directory from config, with a fallback.
func (m *Manager) sessionBaseDir() string {
	if m.config.SessionDir != "" {
		return m.config.SessionDir
	}
	// Derive from SnapshotCachePath: /mnt/data/snapshots → /mnt/data/sessions
	if m.config.SnapshotCachePath != "" {
		return filepath.Join(filepath.Dir(m.config.SnapshotCachePath), "sessions")
	}
	return defaultSessionDir
}

// PauseResult contains the result of a PauseRunner operation.
type PauseResult struct {
	SessionID         string `json:"session_id"`
	Layer             int    `json:"layer"`
	SnapshotSizeBytes int64  `json:"snapshot_size_bytes"`
}

// CheckpointResult contains the result of a CheckpointRunner operation.
type CheckpointResult struct {
	SessionID         string `json:"session_id"`
	Layer             int    `json:"layer"`
	SnapshotSizeBytes int64  `json:"snapshot_size_bytes"`
	Running           bool   `json:"running"`
}

// SessionMetadata is written to each session directory as metadata.json.
type SessionMetadata struct {
	SessionID   string    `json:"session_id"`
	WorkloadKey string    `json:"workload_key"`
	RunnerID    string    `json:"runner_id"`
	HostID      string    `json:"host_id"`
	Layers      int       `json:"layers"`
	CreatedAt   time.Time `json:"created_at"`
	PausedAt    time.Time `json:"paused_at"`
	// RootfsPath is the path to the dirty rootfs overlay for this session.
	RootfsPath string `json:"rootfs_path"`
	// Resource config preserved across pause/resume
	VCPUs    int `json:"vcpus,omitempty"`
	MemoryMB int `json:"memory_mb,omitempty"`
	// TTL config preserved across pause/resume
	TTLSeconds int  `json:"ttl_seconds,omitempty"`
	AutoPause  bool `json:"auto_pause,omitempty"`

	// ServicePort is the port of the user's service inside the VM (from StartCommand).
	// Preserved so that resume can re-forward it into the network namespace.
	ServicePort int `json:"service_port,omitempty"`
	// SnapshotVersion is the snapshot version used to boot this runner.
	SnapshotVersion string `json:"snapshot_version,omitempty"`
	// AccessPlaneConfig preserves the access plane configuration so it can be
	// restored on resume (injected into MMDS).
	AccessPlaneConfig *accessplane.Config `json:"access_plane_config,omitempty"`

	// GCS-backed session fields (populated when SessionChunkBucket is configured).
	// When GCSManifestPath is non-empty, ResumeFromSession uses UFFD-backed
	// GCS chunk loading instead of the host-local LayeredHandler.
	GCSManifestPath     string            `json:"gcs_manifest_path,omitempty"`
	GCSMemIndexObject   string            `json:"gcs_mem_index_object,omitempty"`
	GCSDiskIndexObjects map[string]string `json:"gcs_disk_index_objects,omitempty"` // driveID → GCS path
}

// TryAcquireExec atomically checks that the runner is in a state that allows
// exec and increments ActiveExecs. Returns an error if the runner is
// pausing, suspended, terminated, or quarantined.
func (m *Manager) TryAcquireExec(runnerID string) error {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("runner not found: %s", runnerID)
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()

	switch runner.State {
	case StatePausing, StateSuspended, StateTerminated, StateQuarantined:
		return fmt.Errorf("runner %s is %s", runnerID, runner.State)
	}

	atomic.AddInt32(&runner.ActiveExecs, 1)
	return nil
}

// ReleaseExec decrements ActiveExecs and updates LastExecAt under runner.mu.
func (m *Manager) ReleaseExec(runnerID string) {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return
	}

	runner.mu.Lock()
	atomic.AddInt32(&runner.ActiveExecs, -1)
	runner.LastExecAt = time.Now()
	runner.mu.Unlock()
}

// PauseRunner creates a diff snapshot of the runner's VM, saves session state,
// and destroys the VM. The runner transitions to StateSuspended.
// If syncFS is true, guest filesystems are synced before snapshotting so that
// all data is flushed from the page cache to the block device. This is needed
// when the session may later be resumed via migration (fresh boot without
// the original memory snapshot).
func (m *Manager) PauseRunner(ctx context.Context, runnerID string, syncFS bool) (*PauseResult, error) {
	m.mu.Lock()
	runner, exists := m.runners[runnerID]
	if !exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("runner not found: %s", runnerID)
	}

	vm := m.vms[runnerID]
	// Snapshot the golden metadata under lock — it's written by SetGoldenChunkedMeta
	// from a different goroutine (SyncManifest/heartbeat loop).
	goldenMeta := m.goldenChunkedMeta
	m.mu.Unlock()

	if vm == nil {
		return nil, fmt.Errorf("VM not found for runner %s", runnerID)
	}

	// Take runner.mu to atomically check state+ActiveExecs and transition to
	// StatePausing. This makes pause and exec acquisition mutually exclusive.
	runner.mu.Lock()
	if runner.SessionID == "" {
		runner.mu.Unlock()
		return nil, fmt.Errorf("runner %s has no session_id, cannot pause", runnerID)
	}

	switch runner.State {
	case StatePausing, StateSuspended, StateTerminated:
		runner.mu.Unlock()
		return nil, fmt.Errorf("runner %s is %s, cannot pause", runnerID, runner.State)
	}

	if atomic.LoadInt32(&runner.ActiveExecs) > 0 {
		runner.mu.Unlock()
		return nil, fmt.Errorf("runner %s has active execs, cannot pause", runnerID)
	}

	prePauseState := runner.State
	runner.State = StatePausing
	sessionID := runner.SessionID
	layerN := runner.SessionLayers
	runner.mu.Unlock()

	// Acquire snapshot semaphore to limit concurrent diff snapshot creation.
	// Snapshots are disk I/O heavy (~512MB writes), so we bound concurrency
	// to avoid inflating snapshot latency from disk contention. The semaphore
	// is released immediately after the snapshot completes — GCS uploads use
	// a separate, higher-capacity semaphore (uploadSem).
	select {
	case m.pauseSem <- struct{}{}:
	case <-ctx.Done():
		runner.mu.Lock()
		runner.State = prePauseState
		runner.mu.Unlock()
		return nil, ctx.Err()
	}

	m.logger.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"session_id": sessionID,
		"layer":      layerN,
	}).Info("Pausing runner (creating diff snapshot)")

	// Create session layer directory
	layerDir := filepath.Join(m.sessionBaseDir(), sessionID, fmt.Sprintf("layer_%d", layerN))
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		<-m.pauseSem
		runner.mu.Lock()
		runner.State = prePauseState
		runner.mu.Unlock()
		return nil, fmt.Errorf("failed to create session layer dir: %w", err)
	}

	stateFile := filepath.Join(layerDir, "snapshot.state")
	memDiffFile := filepath.Join(layerDir, "mem_diff.sparse")

	// Flush guest filesystem caches before snapshotting when requested.
	// Without this, data written to extension drives may still be in the guest
	// page cache (ext4 delayed allocation) and not yet on the block device.
	// Normal resume restores memory (including page cache) so it's fine, but
	// migration discards memory and boots fresh — any unflushed data is lost.
	if syncFS && runner.InternalIP != nil {
		syncURL := fmt.Sprintf("http://%s:10501/exec", runner.InternalIP)
		syncBody := `{"command":["sync"]}`
		syncCtx, syncCancel := context.WithTimeout(ctx, 5*time.Second)
		req, _ := http.NewRequestWithContext(syncCtx, "POST", syncURL, strings.NewReader(syncBody))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err != nil {
			m.logger.WithError(err).Warn("Pause: failed to sync guest filesystems (thaw-agent unreachable)")
		} else {
			// The /exec endpoint streams ndjson — we must drain the body
			// to wait for the sync command to actually complete. Without
			// this, resp.Body.Close() returns immediately after headers
			// arrive and the snapshot races with the still-running sync.
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			m.logger.Info("Pause: guest filesystem sync complete")
		}
		syncCancel()
	}

	// Create diff snapshot (pauses VM internally)
	diffStart := time.Now()
	if err := vm.CreateDiffSnapshot(ctx, stateFile, memDiffFile); err != nil {
		<-m.pauseSem
		runner.mu.Lock()
		runner.State = prePauseState
		runner.mu.Unlock()
		return nil, fmt.Errorf("failed to create diff snapshot: %w", err)
	}
	diffDuration := time.Since(diffStart)

	// Calculate snapshot size
	var snapshotSize int64
	if info, err := os.Stat(memDiffFile); err == nil {
		snapshotSize += info.Size()
	}
	if info, err := os.Stat(stateFile); err == nil {
		snapshotSize += info.Size()
	}

	m.logger.WithFields(logrus.Fields{
		"diff_snapshot_ms": diffDuration.Milliseconds(),
		"snapshot_bytes":   snapshotSize,
	}).Info("Pause: diff snapshot created")

	// Release snapshot semaphore — the disk-heavy work is done. Other pauses
	// can now start their snapshots while this one proceeds with GCS uploads.
	<-m.pauseSem

	// Write metadata.json
	sessionDir := filepath.Join(m.sessionBaseDir(), sessionID)

	// Load the previous metadata if it exists — we need GCSMemIndexObject and
	// GCSDiskIndexObjects from a prior pause to use as the base for multi-pause chain dedup.
	var prevGCSMemIndex string
	var prevGCSDiskIndexObjects map[string]string
	if prevData, readErr := os.ReadFile(filepath.Join(sessionDir, "metadata.json")); readErr == nil {
		var prev SessionMetadata
		if json.Unmarshal(prevData, &prev) == nil {
			prevGCSMemIndex = prev.GCSMemIndexObject
			prevGCSDiskIndexObjects = prev.GCSDiskIndexObjects
		}
	}

	metadata := SessionMetadata{
		SessionID:         sessionID,
		WorkloadKey:       runner.WorkloadKey,
		RunnerID:          runnerID,
		HostID:            m.config.HostID,
		Layers:            layerN + 1,
		CreatedAt:         runner.CreatedAt,
		PausedAt:          time.Now(),
		RootfsPath:        runner.RootfsOverlay,
		VCPUs:             runner.Resources.VCPUs,
		MemoryMB:          runner.Resources.MemoryMB,
		TTLSeconds:        runner.TTLSeconds,
		AutoPause:         runner.AutoPause,
		ServicePort:       runner.ServicePort,
		SnapshotVersion:   runner.SnapshotVersion,
		AccessPlaneConfig: runner.AccessPlaneConfig,
	}

	// GCS-backed upload: when sessionMemStore is configured, upload dirty mem
	// diff chunks and VM state to GCS, producing a self-contained SnapshotManifest.
	if m.sessionMemStore != nil {
		gcsBase := fmt.Sprintf("%s/runner_state/%s", runner.WorkloadKey, runnerID)

		uploader := snapshot.NewSessionChunkUploader(m.sessionMemStore, m.sessionDiskStore, m.logger.Logger)

		// Load the base ChunkIndex for memory.
		var baseMemIndex *snapshot.ChunkIndex
		if prevGCSMemIndex != "" {
			prevIdx, dlErr := uploader.DownloadChunkIndex(ctx, prevGCSMemIndex)
			if dlErr != nil {
				m.logger.WithError(dlErr).Warn("Failed to download previous session ChunkIndex; falling back to golden base")
			} else {
				baseMemIndex = prevIdx
			}
		}
		if baseMemIndex == nil && goldenMeta != nil {
			baseMemIndex = snapshot.ChunkIndexFromMeta(goldenMeta)
		}
		if baseMemIndex == nil {
			baseMemIndex = &snapshot.ChunkIndex{
				Version:        "1",
				CreatedAt:      time.Now(),
				ChunkSizeBytes: snapshot.DefaultChunkSize,
			}
			baseMemIndex.CAS.Algo = "sha256"
			baseMemIndex.CAS.Layout = "chunks/mem/{p0}/{hash}"
			baseMemIndex.CAS.Kind = "mem"
			baseMemIndex.Region.Name = "vm_memory"
			baseMemIndex.Region.LogicalSizeBytes = int64(runner.Resources.MemoryMB) * 1024 * 1024
			baseMemIndex.Region.Coverage = "sparse"
			baseMemIndex.Region.DefaultFill = "zero"
		}

		// Parallel phase: merge+upload mem chunks, upload VM state, and upload
		// dirty disk chunks concurrently.
		var newMemIndex *snapshot.ChunkIndex
		newExtDiskIndexes := map[string]*snapshot.ChunkIndex{}
		var newRootfsDiskIndex *snapshot.ChunkIndex
		var extDiskMu sync.Mutex
		vmStateGCSPath := uploader.FullGCSPath(gcsBase + "/snapshot.state")

		uploadStart := time.Now()

		// Acquire upload semaphore to limit concurrent GCS upload phases.
		// Each phase spawns 3-4 streams; with cap=16 that's ~64 concurrent
		// streams — enough parallelism without thrashing the network.
		select {
		case m.uploadSem <- struct{}{}:
		case <-ctx.Done():
			runner.mu.Lock()
			runner.State = prePauseState
			runner.mu.Unlock()
			return nil, ctx.Err()
		}

		g, gCtx := errgroup.WithContext(ctx)

		// Merge and upload memory chunks.
		g.Go(func() error {
			idx, err := uploader.MergeAndUploadMem(gCtx, memDiffFile, baseMemIndex)
			if err != nil {
				return fmt.Errorf("failed to upload session memory chunks to GCS: %w", err)
			}
			// Attach prefetch mapping from UFFD handler if available.
			if handler, ok := m.uffdHandlers[runnerID].(*uffd.Handler); ok {
				if pm := handler.GetPrefetchMapping(); pm != nil {
					idx.PrefetchMapping = pm
					m.logger.WithField("offsets", len(pm.Offsets)).Info("Attached prefetch mapping to ChunkIndex")
				}
			}
			newMemIndex = idx
			return nil
		})

		// Upload VM state.
		g.Go(func() error {
			return uploader.UploadVMState(gCtx, stateFile, vmStateGCSPath)
		})

		// Upload dirty extension disk chunks (one goroutine per dirty drive).
		if m.getDirtyExtensionDiskChunks != nil && m.sessionDiskStore != nil {
			allDirty := m.getDirtyExtensionDiskChunks(runnerID)
			for driveID, dirtyChunks := range allDirty {
				if len(dirtyChunks) == 0 {
					continue
				}
				did := driveID
				dc := dirtyChunks
				g.Go(func() error {
					var baseDiskIndex *snapshot.ChunkIndex
					if prevGCSDiskIndexObjects != nil {
						if prevPath := prevGCSDiskIndexObjects[did]; prevPath != "" {
							prevIdx, dlErr := uploader.DownloadChunkIndex(gCtx, prevPath)
							if dlErr == nil {
								baseDiskIndex = prevIdx
							}
						}
					}
					if baseDiskIndex == nil {
						baseDiskIndex = buildExtensionDriveBaseIndex(goldenMeta, did)
					}
					diskIdx, diskErr := uploader.MergeAndUploadDisk(gCtx, dc, baseDiskIndex)
					if diskErr != nil {
						m.logger.WithError(diskErr).Warnf("GCS disk chunk upload failed for drive %s", did)
						return nil
					}
					extDiskMu.Lock()
					newExtDiskIndexes[did] = diskIdx
					extDiskMu.Unlock()
					return nil
				})
			}
		}

		// Upload dirty rootfs disk chunks.
		if m.getDirtyRootfsDiskChunks != nil && m.sessionDiskStore != nil {
			dirtyRootfs := m.getDirtyRootfsDiskChunks(runnerID)
			if len(dirtyRootfs) > 0 {
				g.Go(func() error {
					var baseRootfsIndex *snapshot.ChunkIndex
					if prevGCSDiskIndexObjects != nil {
						if prevPath := prevGCSDiskIndexObjects["__rootfs__"]; prevPath != "" {
							prevIdx, dlErr := uploader.DownloadChunkIndex(gCtx, prevPath)
							if dlErr == nil {
								baseRootfsIndex = prevIdx
							}
						}
					}
					if baseRootfsIndex == nil {
						baseRootfsIndex = buildRootfsDriveBaseIndex(goldenMeta)
					}
					rootfsIdx, rootfsErr := uploader.MergeAndUploadDisk(gCtx, dirtyRootfs, baseRootfsIndex)
					if rootfsErr != nil {
						m.logger.WithError(rootfsErr).Warn("GCS rootfs disk chunk upload failed")
						return nil
					}
					newRootfsDiskIndex = rootfsIdx
					return nil
				})
			}
		}

		if err := g.Wait(); err != nil {
			<-m.uploadSem
			runner.mu.Lock()
			runner.State = prePauseState
			runner.mu.Unlock()
			return nil, fmt.Errorf("parallel upload failed: %w", err)
		}
		m.logger.WithField("upload_ms", time.Since(uploadStart).Milliseconds()).Info("Pause: parallel uploads complete")

		// Include non-dirty extension drives from golden metadata.
		if goldenMeta != nil {
			for driveID, extDrive := range goldenMeta.ExtensionDrives {
				if _, already := newExtDiskIndexes[driveID]; already {
					continue
				}
				if len(extDrive.Chunks) == 0 {
					continue
				}
				newExtDiskIndexes[driveID] = buildExtensionDriveBaseIndex(goldenMeta, driveID)
			}
		}

		// Always include rootfs in manifest for cross-host resume.
		if newRootfsDiskIndex == nil {
			if prevGCSDiskIndexObjects != nil {
				if prevPath := prevGCSDiskIndexObjects["__rootfs__"]; prevPath != "" {
					prevIdx, dlErr := uploader.DownloadChunkIndex(ctx, prevPath)
					if dlErr == nil {
						newRootfsDiskIndex = prevIdx
					}
				}
			}
			if newRootfsDiskIndex == nil && goldenMeta != nil && len(goldenMeta.RootfsChunks) > 0 {
				newRootfsDiskIndex = buildRootfsDriveBaseIndex(goldenMeta)
			}
		}

		snapshotID := uuid.New().String()
		man := &snapshot.SnapshotManifest{
			Version:     "1",
			SnapshotID:  snapshotID,
			CreatedAt:   time.Now(),
			WorkloadKey: runner.WorkloadKey,
		}
		man.Firecracker.VMStateObject = vmStateGCSPath
		man.Memory.Mode = "chunked"
		man.Memory.TotalSizeBytes = baseMemIndex.Region.LogicalSizeBytes
		man.Memory.ChunkIndexObject = uploader.FullGCSPath(gcsBase + "/chunked-metadata.json")
		man.Integrity.Algo = "sha256"

		if newRootfsDiskIndex != nil {
			man.Disk = snapshot.DiskSection{
				Mode:             "chunked",
				TotalSizeBytes:   newRootfsDiskIndex.Region.LogicalSizeBytes,
				ChunkIndexObject: uploader.FullGCSPath(gcsBase + "/__rootfs__-disk.json"),
			}
		}

		if len(newExtDiskIndexes) > 0 {
			man.ExtensionDisks = make(map[string]snapshot.DiskSection)
			for driveID, diskIdx := range newExtDiskIndexes {
				man.ExtensionDisks[driveID] = snapshot.DiskSection{
					Mode:             "chunked",
					TotalSizeBytes:   diskIdx.Region.LogicalSizeBytes,
					ChunkIndexObject: uploader.FullGCSPath(gcsBase + "/" + driveID + "-disk.json"),
				}
			}
		}

		allDiskIndexes := make(map[string]*snapshot.ChunkIndex, len(newExtDiskIndexes)+1)
		for k, v := range newExtDiskIndexes {
			allDiskIndexes[k] = v
		}
		if newRootfsDiskIndex != nil {
			allDiskIndexes["__rootfs__"] = newRootfsDiskIndex
		}

		if writeErr := uploader.WriteManifestWithExtensions(ctx, gcsBase, man, newMemIndex, allDiskIndexes); writeErr != nil {
			<-m.uploadSem
			runner.mu.Lock()
			runner.State = prePauseState
			runner.mu.Unlock()
			return nil, fmt.Errorf("failed to write session manifest to GCS: %w", writeErr)
		}

		metadata.GCSManifestPath = uploader.FullGCSPath(gcsBase + "/snapshot_manifest.json")
		metadata.GCSMemIndexObject = uploader.FullGCSPath(gcsBase + "/chunked-metadata.json")
		if len(prevGCSDiskIndexObjects) > 0 || len(allDiskIndexes) > 0 {
			if metadata.GCSDiskIndexObjects == nil {
				metadata.GCSDiskIndexObjects = make(map[string]string)
			}
			for driveID, path := range prevGCSDiskIndexObjects {
				if _, dirty := allDiskIndexes[driveID]; !dirty {
					metadata.GCSDiskIndexObjects[driveID] = path
				}
			}
			for driveID := range allDiskIndexes {
				metadata.GCSDiskIndexObjects[driveID] = uploader.FullGCSPath(gcsBase + "/" + driveID + "-disk.json")
			}
		}
		m.logger.WithFields(logrus.Fields{
			"runner_id":    runnerID,
			"gcs_manifest": metadata.GCSManifestPath,
		}).Info("Session uploaded to GCS successfully (diff_file mode)")
	}
	metadataBytes, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata.json"), metadataBytes, 0644); err != nil {
		m.logger.WithError(err).Warn("Failed to write session metadata")
	}

	// Upload metadata to GCS for cross-host resume.
	if m.sessionMemStore != nil && metadata.GCSManifestPath != "" {
		uploader := snapshot.NewSessionChunkUploader(m.sessionMemStore, m.sessionDiskStore, m.logger.Logger)
		if uploadErr := uploader.UploadSessionMetadata(ctx, runner.WorkloadKey, runnerID, metadataBytes); uploadErr != nil {
			m.logger.WithError(uploadErr).Warn("Failed to upload session metadata to GCS")
		}
	}

	// Release upload semaphore — all GCS I/O is done.
	if m.sessionMemStore != nil {
		<-m.uploadSem
	}

	// Update state to Suspended and prepare the result BEFORE teardown,
	// so the gRPC response returns immediately. Teardown runs in background.
	m.mu.Lock()
	if slot, ok := m.runnerToSlot[runnerID]; ok {
		delete(m.slotToRunner, slot)
		delete(m.runnerToSlot, runnerID)
	}
	os.Remove(filepath.Join(m.config.SocketDir, runnerID+".sock"))
	runner.State = StateSuspended
	runner.SessionLayers = layerN + 1
	runner.SessionDir = sessionDir
	runner.PausedAt = time.Now()

	// Extract resources from maps under lock for background teardown.
	delete(m.vms, runnerID)
	handler := m.uffdHandlers[runnerID]
	delete(m.uffdHandlers, runnerID)
	m.mu.Unlock()

	// Background teardown: vm.Stop, handler.Stop, ns.Release, and FUSE
	// cleanup run concurrently in a background goroutine. None of these
	// affect the PauseResult or GCS state — they only free host resources.
	go func() {
		teardownStart := time.Now()
		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			defer wg.Done()
			m.logger.WithField("runner_id", runnerID).Debug("Pause teardown: stopping VM")
			vm.Stop()
			m.logger.WithFields(logrus.Fields{
				"runner_id":  runnerID,
				"vm_stop_ms": time.Since(teardownStart).Milliseconds(),
			}).Debug("Pause teardown: VM stopped")
		}()

		if handler != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				handlerStart := time.Now()
				m.logger.WithField("runner_id", runnerID).Debug("Pause teardown: stopping UFFD handler")
				handler.Stop()
				m.logger.WithFields(logrus.Fields{
					"runner_id":       runnerID,
					"handler_stop_ms": time.Since(handlerStart).Milliseconds(),
				}).Debug("Pause teardown: UFFD handler stopped")
			}()
		}

		if m.netnsNetwork != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				nsStart := time.Now()
				m.logger.WithField("runner_id", runnerID).Debug("Pause teardown: releasing network namespace")
				m.netnsNetwork.ReleaseNamespace(runnerID)
				m.logger.WithFields(logrus.Fields{
					"runner_id":     runnerID,
					"ns_release_ms": time.Since(nsStart).Milliseconds(),
				}).Debug("Pause teardown: network namespace released")
			}()
		}

		// FUSE cleanup: unmount disks after VM stop completes.
		if m.cleanupFUSEDisks != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fuseStart := time.Now()
				m.logger.WithField("runner_id", runnerID).Debug("Pause teardown: cleaning up FUSE disks")
				m.cleanupFUSEDisks(runnerID)
				m.logger.WithFields(logrus.Fields{
					"runner_id":       runnerID,
					"fuse_cleanup_ms": time.Since(fuseStart).Milliseconds(),
				}).Debug("Pause teardown: FUSE disks cleaned up")
			}()
		}

		wg.Wait()
		m.logger.WithFields(logrus.Fields{
			"runner_id":   runnerID,
			"teardown_ms": time.Since(teardownStart).Milliseconds(),
		}).Debug("Pause teardown: complete")
	}()

	m.logger.WithFields(logrus.Fields{
		"runner_id":     runnerID,
		"session_id":    sessionID,
		"layer":         layerN,
		"snapshot_size": snapshotSize,
	}).Info("Runner paused successfully")

	return &PauseResult{
		SessionID:         sessionID,
		Layer:             layerN,
		SnapshotSizeBytes: snapshotSize,
	}, nil
}

// CheckpointRunner creates a non-destructive snapshot: the VM is paused briefly
// while the diff snapshot is taken, then resumed. The session state is uploaded
// to GCS (if configured) so it can be used for cross-host resume later, but the
// VM keeps running and the runner stays in its current state.
func (m *Manager) CheckpointRunner(ctx context.Context, runnerID string) (*CheckpointResult, error) {
	m.mu.Lock()
	runner, exists := m.runners[runnerID]
	if !exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("runner not found: %s", runnerID)
	}

	if runner.SessionID == "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("runner %s has no session_id, cannot checkpoint", runnerID)
	}

	if runner.State != StateIdle && runner.State != StateBusy {
		m.mu.Unlock()
		return nil, fmt.Errorf("runner %s is in state %s, cannot checkpoint", runnerID, runner.State)
	}

	vm := m.vms[runnerID]
	if vm == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("VM not found for runner %s", runnerID)
	}

	sessionID := runner.SessionID
	layerN := runner.SessionLayers
	goldenMeta := m.goldenChunkedMeta
	m.mu.Unlock()

	m.logger.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"session_id": sessionID,
		"layer":      layerN,
	}).Info("Checkpointing runner (non-destructive snapshot)")

	// Create checkpoint layer directory
	layerDir := filepath.Join(m.sessionBaseDir(), sessionID, fmt.Sprintf("layer_%d", layerN))
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint layer dir: %w", err)
	}

	stateFile := filepath.Join(layerDir, "snapshot.state")
	memDiffFile := filepath.Join(layerDir, "mem_diff.sparse")

	// Create non-destructive diff snapshot (pauses VM, takes snapshot, resumes VM)
	if err := vm.CreateDiffSnapshotNonDestructive(ctx, stateFile, memDiffFile); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint snapshot: %w", err)
	}

	// Calculate snapshot size
	var snapshotSize int64
	if info, err := os.Stat(memDiffFile); err == nil {
		snapshotSize += info.Size()
	}
	if info, err := os.Stat(stateFile); err == nil {
		snapshotSize += info.Size()
	}

	// Write metadata.json (same format as PauseRunner but runner stays active)
	sessionDir := filepath.Join(m.sessionBaseDir(), sessionID)

	var prevGCSMemIndex string
	var prevGCSDiskIndexObjects map[string]string
	if prevData, readErr := os.ReadFile(filepath.Join(sessionDir, "metadata.json")); readErr == nil {
		var prev SessionMetadata
		if json.Unmarshal(prevData, &prev) == nil {
			prevGCSMemIndex = prev.GCSMemIndexObject
			prevGCSDiskIndexObjects = prev.GCSDiskIndexObjects
		}
	}

	metadata := SessionMetadata{
		SessionID:         sessionID,
		WorkloadKey:       runner.WorkloadKey,
		RunnerID:          runnerID,
		HostID:            m.config.HostID,
		Layers:            layerN + 1,
		CreatedAt:         runner.CreatedAt,
		PausedAt:          time.Now(),
		RootfsPath:        runner.RootfsOverlay,
		VCPUs:             runner.Resources.VCPUs,
		MemoryMB:          runner.Resources.MemoryMB,
		TTLSeconds:        runner.TTLSeconds,
		AutoPause:         runner.AutoPause,
		ServicePort:       runner.ServicePort,
		SnapshotVersion:   runner.SnapshotVersion,
		AccessPlaneConfig: runner.AccessPlaneConfig,
	}

	// GCS-backed upload (same logic as PauseRunner)
	if m.sessionMemStore != nil {
		gcsBase := fmt.Sprintf("%s/runner_state/%s", runner.WorkloadKey, runnerID)
		uploader := snapshot.NewSessionChunkUploader(m.sessionMemStore, m.sessionDiskStore, m.logger.Logger)

		var baseMemIndex *snapshot.ChunkIndex
		if prevGCSMemIndex != "" {
			prevIdx, dlErr := uploader.DownloadChunkIndex(ctx, prevGCSMemIndex)
			if dlErr != nil {
				m.logger.WithError(dlErr).Warn("Checkpoint: failed to download previous session ChunkIndex")
			} else {
				baseMemIndex = prevIdx
			}
		}
		if baseMemIndex == nil && goldenMeta != nil {
			baseMemIndex = snapshot.ChunkIndexFromMeta(goldenMeta)
		}
		if baseMemIndex == nil {
			baseMemIndex = &snapshot.ChunkIndex{
				Version:        "1",
				CreatedAt:      time.Now(),
				ChunkSizeBytes: snapshot.DefaultChunkSize,
			}
			baseMemIndex.CAS.Algo = "sha256"
			baseMemIndex.CAS.Layout = "chunks/mem/{p0}/{hash}"
			baseMemIndex.CAS.Kind = "mem"
			baseMemIndex.Region.Name = "vm_memory"
			baseMemIndex.Region.LogicalSizeBytes = int64(runner.Resources.MemoryMB) * 1024 * 1024
			baseMemIndex.Region.Coverage = "sparse"
			baseMemIndex.Region.DefaultFill = "zero"
		}

		newMemIndex, err := uploader.MergeAndUploadMem(ctx, memDiffFile, baseMemIndex)
		if err != nil {
			return nil, fmt.Errorf("checkpoint: failed to upload memory chunks to GCS: %w", err)
		} else {
			// Attach prefetch mapping from UFFD handler if available.
			if handler, ok := m.uffdHandlers[runnerID].(*uffd.Handler); ok {
				if pm := handler.GetPrefetchMapping(); pm != nil {
					newMemIndex.PrefetchMapping = pm
					m.logger.WithField("offsets", len(pm.Offsets)).Info("Checkpoint: attached prefetch mapping to ChunkIndex")
				}
			}

			vmStateGCSPath := uploader.FullGCSPath(gcsBase + "/snapshot.state")
			if uploadErr := uploader.UploadVMState(ctx, stateFile, vmStateGCSPath); uploadErr != nil {
				return nil, fmt.Errorf("checkpoint: failed to upload VM state to GCS: %w", uploadErr)
			} else {
				// Upload extension disk chunks
				newExtDiskIndexes := map[string]*snapshot.ChunkIndex{}
				if m.getDirtyExtensionDiskChunks != nil && m.sessionDiskStore != nil {
					allDirty := m.getDirtyExtensionDiskChunks(runnerID)
					for driveID, dirtyChunks := range allDirty {
						if len(dirtyChunks) == 0 {
							continue
						}
						var baseDiskIndex *snapshot.ChunkIndex
						if prevGCSDiskIndexObjects != nil {
							if prevPath := prevGCSDiskIndexObjects[driveID]; prevPath != "" {
								prevIdx, dlErr := uploader.DownloadChunkIndex(ctx, prevPath)
								if dlErr == nil {
									baseDiskIndex = prevIdx
								}
							}
						}
						if baseDiskIndex == nil {
							baseDiskIndex = buildExtensionDriveBaseIndex(goldenMeta, driveID)
						}
						diskIdx, diskErr := uploader.MergeAndUploadDisk(ctx, dirtyChunks, baseDiskIndex)
						if diskErr == nil {
							newExtDiskIndexes[driveID] = diskIdx
						}
					}
				}

				// Include non-dirty extension drives from golden metadata so the
				// manifest is self-contained.
				if goldenMeta != nil {
					for driveID, extDrive := range goldenMeta.ExtensionDrives {
						if _, already := newExtDiskIndexes[driveID]; already {
							continue
						}
						if len(extDrive.Chunks) == 0 {
							continue
						}
						newExtDiskIndexes[driveID] = buildExtensionDriveBaseIndex(goldenMeta, driveID)
					}
				}

				// Upload rootfs disk chunks
				var newRootfsDiskIndex *snapshot.ChunkIndex
				if m.getDirtyRootfsDiskChunks != nil && m.sessionDiskStore != nil {
					dirtyRootfs := m.getDirtyRootfsDiskChunks(runnerID)
					if len(dirtyRootfs) > 0 {
						var baseRootfsIndex *snapshot.ChunkIndex
						if prevGCSDiskIndexObjects != nil {
							if prevPath := prevGCSDiskIndexObjects["__rootfs__"]; prevPath != "" {
								prevIdx, dlErr := uploader.DownloadChunkIndex(ctx, prevPath)
								if dlErr == nil {
									baseRootfsIndex = prevIdx
								}
							}
						}
						if baseRootfsIndex == nil {
							baseRootfsIndex = buildRootfsDriveBaseIndex(goldenMeta)
						}
						rootfsIdx, rootfsErr := uploader.MergeAndUploadDisk(ctx, dirtyRootfs, baseRootfsIndex)
						if rootfsErr == nil {
							newRootfsDiskIndex = rootfsIdx
						}
					}
				}

				// Always include rootfs in GCS manifest for cross-host resume.
				if newRootfsDiskIndex == nil {
					if prevGCSDiskIndexObjects != nil {
						if prevPath := prevGCSDiskIndexObjects["__rootfs__"]; prevPath != "" {
							prevIdx, dlErr := uploader.DownloadChunkIndex(ctx, prevPath)
							if dlErr == nil {
								newRootfsDiskIndex = prevIdx
							}
						}
					}
					if newRootfsDiskIndex == nil && goldenMeta != nil && len(goldenMeta.RootfsChunks) > 0 {
						newRootfsDiskIndex = buildRootfsDriveBaseIndex(goldenMeta)
					}
				}

				snapshotID := uuid.New().String()
				man := &snapshot.SnapshotManifest{
					Version:     "1",
					SnapshotID:  snapshotID,
					CreatedAt:   time.Now(),
					WorkloadKey: runner.WorkloadKey,
				}
				man.Firecracker.VMStateObject = vmStateGCSPath
				man.Memory.Mode = "chunked"
				man.Memory.TotalSizeBytes = baseMemIndex.Region.LogicalSizeBytes
				man.Memory.ChunkIndexObject = uploader.FullGCSPath(gcsBase + "/chunked-metadata.json")
				man.Integrity.Algo = "sha256"

				if newRootfsDiskIndex != nil {
					man.Disk = snapshot.DiskSection{
						Mode:             "chunked",
						TotalSizeBytes:   newRootfsDiskIndex.Region.LogicalSizeBytes,
						ChunkIndexObject: uploader.FullGCSPath(gcsBase + "/__rootfs__-disk.json"),
					}
				}

				if len(newExtDiskIndexes) > 0 {
					man.ExtensionDisks = make(map[string]snapshot.DiskSection)
					for driveID, diskIdx := range newExtDiskIndexes {
						man.ExtensionDisks[driveID] = snapshot.DiskSection{
							Mode:             "chunked",
							TotalSizeBytes:   diskIdx.Region.LogicalSizeBytes,
							ChunkIndexObject: uploader.FullGCSPath(gcsBase + "/" + driveID + "-disk.json"),
						}
					}
				}

				allDiskIndexes := make(map[string]*snapshot.ChunkIndex, len(newExtDiskIndexes)+1)
				for k, v := range newExtDiskIndexes {
					allDiskIndexes[k] = v
				}
				if newRootfsDiskIndex != nil {
					allDiskIndexes["__rootfs__"] = newRootfsDiskIndex
				}

				if writeErr := uploader.WriteManifestWithExtensions(ctx, gcsBase, man, newMemIndex, allDiskIndexes); writeErr != nil {
					return nil, fmt.Errorf("checkpoint: failed to write manifest to GCS: %w", writeErr)
				} else {
					metadata.GCSManifestPath = uploader.FullGCSPath(gcsBase + "/snapshot_manifest.json")
					metadata.GCSMemIndexObject = uploader.FullGCSPath(gcsBase + "/chunked-metadata.json")
					if len(prevGCSDiskIndexObjects) > 0 || len(allDiskIndexes) > 0 {
						if metadata.GCSDiskIndexObjects == nil {
							metadata.GCSDiskIndexObjects = make(map[string]string)
						}
						for driveID, path := range prevGCSDiskIndexObjects {
							if _, dirty := allDiskIndexes[driveID]; !dirty {
								metadata.GCSDiskIndexObjects[driveID] = path
							}
						}
						for driveID := range allDiskIndexes {
							metadata.GCSDiskIndexObjects[driveID] = uploader.FullGCSPath(gcsBase + "/" + driveID + "-disk.json")
						}
					}
					m.logger.WithFields(logrus.Fields{
						"runner_id":    runnerID,
						"gcs_manifest": metadata.GCSManifestPath,
					}).Info("Checkpoint uploaded to GCS successfully")
				}
			}
		}
	}

	metadataBytes, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata.json"), metadataBytes, 0644); err != nil {
		m.logger.WithError(err).Warn("Failed to write checkpoint metadata")
	}

	// Upload metadata to GCS for cross-host resume.
	if m.sessionMemStore != nil && metadata.GCSManifestPath != "" {
		uploader := snapshot.NewSessionChunkUploader(m.sessionMemStore, m.sessionDiskStore, m.logger.Logger)
		if uploadErr := uploader.UploadSessionMetadata(ctx, runner.WorkloadKey, runnerID, metadataBytes); uploadErr != nil {
			m.logger.WithError(uploadErr).Warn("Failed to upload checkpoint metadata to GCS")
		}
	}

	// Increment session layers (VM stays running)
	m.mu.Lock()
	runner.SessionLayers = layerN + 1
	m.mu.Unlock()

	m.logger.WithFields(logrus.Fields{
		"runner_id":     runnerID,
		"session_id":    sessionID,
		"layer":         layerN,
		"snapshot_size": snapshotSize,
	}).Info("Runner checkpointed successfully (VM still running)")

	return &CheckpointResult{
		SessionID:         sessionID,
		Layer:             layerN,
		SnapshotSizeBytes: snapshotSize,
		Running:           true,
	}, nil
}

// ResumeFromSession restores a runner from a session snapshot using layered UFFD.
// runnerID is optional — when provided (cross-host resume via RPC), it enables
// downloading session metadata from GCS if not found locally.
// attestationToken is optional — when provided, it's injected into MMDS for access plane auth.
func (m *Manager) ResumeFromSession(ctx context.Context, sessionID, workloadKey, runnerID, attestationToken string) (*Runner, error) {
	sessionDir := filepath.Join(m.sessionBaseDir(), sessionID)

	// Read metadata — try local disk first, fall back to GCS for cross-host resume.
	metadataBytes, err := os.ReadFile(filepath.Join(sessionDir, "metadata.json"))
	if err != nil && m.sessionMemStore != nil && runnerID != "" && workloadKey != "" {
		// Local metadata not found — try downloading from GCS (cross-host resume).
		uploader := snapshot.NewSessionChunkUploader(m.sessionMemStore, m.sessionDiskStore, m.logger.Logger)
		gcsData, dlErr := uploader.DownloadSessionMetadata(ctx, workloadKey, runnerID)
		if dlErr != nil {
			return nil, fmt.Errorf("session %s not found locally or in GCS: local=%w, gcs=%v", sessionID, err, dlErr)
		}
		metadataBytes = gcsData
		m.logger.WithField("session_id", sessionID).Info("Downloaded session metadata from GCS (cross-host resume)")
		// Create local session dir and cache metadata for the rest of the resume flow.
		if mkErr := os.MkdirAll(sessionDir, 0755); mkErr != nil {
			return nil, fmt.Errorf("failed to create session dir: %w", mkErr)
		}
		if writeErr := os.WriteFile(filepath.Join(sessionDir, "metadata.json"), metadataBytes, 0644); writeErr != nil {
			m.logger.WithError(writeErr).Warn("Failed to cache session metadata locally")
		}
	} else if err != nil {
		return nil, fmt.Errorf("session %s not found: %w", sessionID, err)
	}

	var metadata SessionMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return nil, fmt.Errorf("invalid session metadata: %w", err)
	}

	if workloadKey != "" && metadata.WorkloadKey != workloadKey {
		return nil, fmt.Errorf("session %s workload_key mismatch: session=%q, request=%q", sessionID, metadata.WorkloadKey, workloadKey)
	}

	if metadata.Layers == 0 {
		return nil, fmt.Errorf("session %s has no layers", sessionID)
	}

	// Always use the runner ID from session metadata (authoritative source).
	runnerID = metadata.RunnerID

	// Parallelize lease acquisition + kernel path lookup with GCS manifest
	// download. These are independent operations and the GCS round-trip is
	// the slower of the two.
	var lease *bringupLease
	var kernelPath string
	var man *snapshot.SnapshotManifest
	var uploader *snapshot.SessionChunkUploader

	setupG, setupCtx := errgroup.WithContext(ctx)

	setupG.Go(func() error {
		var leaseErr error
		lease, leaseErr = m.AcquireBringupLease(runnerID, sessionID)
		if leaseErr != nil {
			return leaseErr
		}
		var kernErr error
		kernelPath, kernErr = m.snapshotCache.GetKernelPath()
		if kernErr != nil {
			lease.Release()
			lease = nil
			return fmt.Errorf("failed to get kernel path: %w", kernErr)
		}
		return nil
	})

	if metadata.GCSManifestPath != "" && m.sessionMemStore != nil {
		uploader = snapshot.NewSessionChunkUploader(m.sessionMemStore, m.sessionDiskStore, m.logger.Logger)
		setupG.Go(func() error {
			var dlErr error
			man, dlErr = uploader.DownloadManifest(setupCtx, metadata.GCSManifestPath)
			if dlErr != nil {
				return fmt.Errorf("failed to download session manifest: %w", dlErr)
			}
			return nil
		})
	}

	if err := setupG.Wait(); err != nil {
		if lease != nil {
			lease.Release()
		}
		return nil, err
	}
	defer lease.Release()

	m.logger.WithFields(logrus.Fields{
		"session_id":   sessionID,
		"layers":       metadata.Layers,
		"workload_key": metadata.WorkloadKey,
	}).Info("Resuming runner from session snapshot")

	tap := lease.Tap
	nsInfo := lease.NsInfo

	// Use the session's rootfs overlay (preserved during pause)
	overlayPath := metadata.RootfsPath

	// Build the UFFD handler and state file path, using GCS-backed chunks when
	// available (metadata.GCSManifestPath is set) or falling back to the local
	// LayeredHandler (requires golden snapshot.mem on this host).
	uffdSocketPath := filepath.Join(m.config.SocketDir, runnerID+"-uffd.sock")
	extensionDrivePaths := map[string]string{}
	var uffdHandler uffdStopper
	var latestStateFile string
	var sessionPrefetcher *uffd.Prefetcher

	// Extract tenant ID for cache isolation.
	tenantID := ""
	if metadata.AccessPlaneConfig != nil {
		tenantID = metadata.AccessPlaneConfig.TenantID
	}

	if man != nil && uploader != nil {
		// GCS-backed resume: manifest already downloaded in parallel above.
		// Download ChunkIndex, convert to ChunkedSnapshotMetadata,
		// create a Handler that fetches pages lazily from GCS.

		// Phase A: Parallel downloads + FUSE setup + Firecracker process start.
		// These are structured to maximize overlap:
		//  - Mem index download → UFFD handler creation (sequential dependency)
		//  - Disk index downloads → FUSE mounts (sequential dependency per drive)
		//  - VM state download → needed for LoadSnapshot only
		//  - Firecracker process start → needs netns only, overlaps with everything
		//  - Port forwarding → needs netns only, overlaps with everything
		var memIdx *snapshot.ChunkIndex
		var rootfsDiskIdx *snapshot.ChunkIndex
		extDiskIndexes := map[string]*snapshot.ChunkIndex{}
		var extDiskMu sync.Mutex

		localStateDir := filepath.Join(m.config.SocketDir, "session-state")
		if mkdirErr := os.MkdirAll(localStateDir, 0755); mkdirErr != nil {
			return nil, fmt.Errorf("failed to create local state dir: %w", mkdirErr)
		}
		latestStateFile = filepath.Join(localStateDir, runnerID+".state")

		// memReady signals when the memory chunk index is downloaded and the
		// UFFD handler is created + started. LoadSnapshot needs this.
		memReady := make(chan struct{})
		var gcsHandler *uffd.Handler

		setupG, setupCtx := errgroup.WithContext(ctx)

		// Download mem chunk index → create UFFD handler → start prefetcher.
		setupG.Go(func() error {
			var dlErr error
			memIdx, dlErr = uploader.DownloadChunkIndex(setupCtx, man.Memory.ChunkIndexObject)
			if dlErr != nil {
				return fmt.Errorf("failed to download mem chunk index: %w", dlErr)
			}

			chunkedMeta := snapshot.ChunkIndexToMetadata(memIdx)
			var handlerErr error
			gcsHandler, handlerErr = uffd.NewHandler(uffd.HandlerConfig{
				SocketPath:             uffdSocketPath,
				ChunkStore:             m.sessionMemStore,
				Metadata:               chunkedMeta,
				Logger:                 m.logger.Logger,
				FaultConcurrency:       32,
				EnablePrefetchTracking: true,
			})
			if handlerErr != nil {
				return fmt.Errorf("failed to create GCS UFFD handler: %w", handlerErr)
			}
			if startErr := gcsHandler.Start(); startErr != nil {
				gcsHandler.Stop()
				return fmt.Errorf("failed to start GCS UFFD handler: %w", startErr)
			}

			// Start prefetcher if a recorded access-pattern mapping exists.
			if chunkedMeta.MemPrefetchMapping != nil && len(chunkedMeta.MemPrefetchMapping.Offsets) > 0 {
				sessionPrefetcher = uffd.NewPrefetcher(uffd.PrefetcherConfig{
					Mapping:     chunkedMeta.MemPrefetchMapping,
					ChunkStore:  m.sessionMemStore,
					Metadata:    chunkedMeta,
					Connected:   gcsHandler.Connected(),
					Logger:      m.logger.Logger,
					CopyWorkers: 16,
				})
				gcsHandler.SetPrefetcher(sessionPrefetcher)
				sessionPrefetcher.Start()
				m.logger.WithField("pages", len(chunkedMeta.MemPrefetchMapping.Offsets)).Info("Started access-pattern prefetcher")
			}

			close(memReady)
			return nil
		})

		// Download VM state (needed for LoadSnapshot, runs in parallel).
		setupG.Go(func() error {
			if dlErr := uploader.DownloadVMState(setupCtx, man.Firecracker.VMStateObject, latestStateFile); dlErr != nil {
				return fmt.Errorf("failed to download VM state from GCS: %w", dlErr)
			}
			return nil
		})

		// Download disk indexes → FUSE mount (each drive is independent).
		if m.setupExtensionFUSEDisk != nil {
			for driveID, diskSection := range man.ExtensionDisks {
				if diskSection.Mode != "chunked" || diskSection.ChunkIndexObject == "" {
					continue
				}
				did := driveID
				obj := diskSection.ChunkIndexObject
				setupG.Go(func() error {
					diskIdx, diskDlErr := uploader.DownloadChunkIndex(setupCtx, obj)
					if diskDlErr != nil {
						return fmt.Errorf("failed to download disk chunk index for drive %s: %w", did, diskDlErr)
					}
					extDiskMu.Lock()
					extDiskIndexes[did] = diskIdx
					extDiskMu.Unlock()
					// Mount FUSE disk immediately after download — no need to wait for other indexes.
					diskRefs := snapshot.ChunkIndexToRefs(diskIdx)
					fusePath, fuseErr := m.setupExtensionFUSEDisk(runnerID, did, diskRefs, diskIdx.Region.LogicalSizeBytes, diskIdx.ChunkSizeBytes, tenantID)
					if fuseErr != nil {
						return fmt.Errorf("failed to setup FUSE disk for drive %s session resume: %w", did, fuseErr)
					}
					extDiskMu.Lock()
					extensionDrivePaths[did] = fusePath
					extDiskMu.Unlock()
					return nil
				})
			}
		}

		// Download rootfs disk index → FUSE mount.
		if m.setupRootfsFUSEDisk != nil && man.Disk.Mode == "chunked" && man.Disk.ChunkIndexObject != "" {
			setupG.Go(func() error {
				var diskDlErr error
				rootfsDiskIdx, diskDlErr = uploader.DownloadChunkIndex(setupCtx, man.Disk.ChunkIndexObject)
				if diskDlErr != nil {
					return fmt.Errorf("failed to download rootfs disk chunk index: %w", diskDlErr)
				}
				rootfsRefs := snapshot.ChunkIndexToRefs(rootfsDiskIdx)
				rootfsFusePath, fuseErr := m.setupRootfsFUSEDisk(runnerID, rootfsRefs, rootfsDiskIdx.Region.LogicalSizeBytes, rootfsDiskIdx.ChunkSizeBytes, tenantID)
				if fuseErr != nil {
					return fmt.Errorf("failed to setup FUSE rootfs disk for session resume: %w", fuseErr)
				}
				overlayPath = rootfsFusePath
				m.logger.WithField("rootfs_fuse_path", rootfsFusePath).Info("Using FUSE-backed rootfs for cross-host session resume")
				return nil
			})
		}

		// Port forwarding — only needs network namespace, not VM.
		if m.netnsNetwork != nil {
			setupG.Go(func() error {
				if err := m.netnsNetwork.ForwardPort(runnerID, snapshot.ThawAgentHealthPort); err != nil {
					m.logger.WithError(err).Warn("Failed to forward capsule-thaw-agent health port")
				}
				if err := m.netnsNetwork.ForwardPort(runnerID, snapshot.ThawAgentDebugPort); err != nil {
					m.logger.WithError(err).Warn("Failed to forward capsule-thaw-agent debug port")
				}
				if metadata.ServicePort > 0 {
					if err := m.netnsNetwork.ForwardPort(runnerID, metadata.ServicePort); err != nil {
						m.logger.WithField("port", metadata.ServicePort).WithError(err).Warn("Failed to forward service port on resume")
					}
				}
				return nil
			})
		}

		if setupErr := setupG.Wait(); setupErr != nil {
			if gcsHandler != nil {
				gcsHandler.Stop()
			}
			return nil, setupErr
		}

		uffdHandler = gcsHandler

		m.logger.WithFields(logrus.Fields{
			"session_id":  sessionID,
			"gcs_vmstate": man.Firecracker.VMStateObject,
			"rootfs":      overlayPath,
		}).Info("Resuming from GCS-backed session (UFFD chunked)")
	} else if m.sessionMemStore != nil {
		// Chunked mode but no GCS manifest — the GCS upload during pause must
		// have failed. Local resume is not possible in chunked mode because
		// rootfs.img and snapshot.mem are not stored locally.
		return nil, fmt.Errorf("session %s has no GCS manifest (GCS upload likely failed during pause); "+
			"local resume is not supported in chunked mode", sessionID)
	} else {
		// Local fallback (non-chunked mode): LayeredHandler uses golden
		// snapshot.mem on this host.
		goldenMemPath := filepath.Join(m.config.SnapshotCachePath, "snapshot.mem")
		if _, err := os.Stat(goldenMemPath); err != nil {
			return nil, fmt.Errorf("golden snapshot.mem not found for local resume: %w", err)
		}

		// Build diff layer paths (oldest first).
		var diffLayers []string
		for i := 0; i < metadata.Layers; i++ {
			layerPath := filepath.Join(sessionDir, fmt.Sprintf("layer_%d", i), "mem_diff.sparse")
			if _, err := os.Stat(layerPath); err == nil {
				diffLayers = append(diffLayers, layerPath)
			}
		}

		latestStateFile = filepath.Join(sessionDir, fmt.Sprintf("layer_%d", metadata.Layers-1), "snapshot.state")

		layeredHandler, handlerErr := uffd.NewLayeredHandler(uffd.LayeredHandlerConfig{
			SocketPath:       uffdSocketPath,
			GoldenMemPath:    goldenMemPath,
			DiffLayers:       diffLayers,
			Logger:           m.logger.Logger,
			FaultConcurrency: 32,
		})
		if handlerErr != nil {
			return nil, fmt.Errorf("failed to create layered UFFD handler: %w", handlerErr)
		}
		if startErr := layeredHandler.Start(); startErr != nil {
			layeredHandler.Stop()
			return nil, fmt.Errorf("failed to start UFFD handler: %w", startErr)
		}
		uffdHandler = layeredHandler
	}

	// Set up network config
	internalIP := nsInfo.HostReachableIP

	// Create VM config
	vmCfg := firecracker.VMConfig{
		VMID:           runnerID,
		SocketDir:      m.config.SocketDir,
		FirecrackerBin: m.config.FirecrackerBin,
		KernelPath:     kernelPath,
		RootfsPath:     overlayPath,
		VCPUs:          metadata.VCPUs,
		MemoryMB:       metadata.MemoryMB,
		NetworkIface: &firecracker.NetworkInterface{
			IfaceID:     "eth0",
			HostDevName: tap.Name,
			GuestMAC:    tap.MAC,
		},
		Drives:  m.buildDrives(extensionDrivePaths),
		LogPath: filepath.Join(m.config.LogDir, runnerID+".log"),
	}

	vmCfg.NetNSPath = nsInfo.GetFirecrackerNetNSPath()

	// Set up per-runner snapshot symlinks before creating the VM so the
	// SnapshotDir is available for the private mount namespace.
	snapshotDir, symlinkCleanup, symlinkErr := m.setupSnapshotSymlinks(runnerID, overlayPath, extensionDrivePaths)
	if symlinkErr != nil {
		uffdHandler.Stop()
		return nil, fmt.Errorf("failed to setup snapshot symlinks: %w", symlinkErr)
	}
	defer symlinkCleanup()

	vmCfg.SnapshotDir = snapshotDir

	vm, err := firecracker.NewVM(vmCfg, m.logger.Logger)
	if err != nil {
		uffdHandler.Stop()
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// Restore from snapshot with UFFD (paused — don't resume yet).
	// With per-VM namespaces, tap-slot-0 already exists — no TAP rename needed.
	restoreErr := vm.RestoreFromSnapshotWithUFFD(ctx, latestStateFile, uffdSocketPath, false)

	if restoreErr != nil {
		uffdHandler.Stop()
		vm.Stop()
		return nil, fmt.Errorf("failed to restore from session snapshot: %w", restoreErr)
	}

	// Clean up downloaded state file — Firecracker holds the fd open,
	// so removing the directory entry is safe and avoids accumulation.
	if latestStateFile != "" {
		os.Remove(latestStateFile)
	}

	// Prefetch is fire-and-forget: the prefetcher started during setup and
	// loads pages in background via UFFD. Any pages not yet prefetched will
	// be demand-fetched on page fault — the VM is fully functional regardless.
	// (Matches infra's approach: no blocking wait before resume.)

	// Build runner record
	runner := &Runner{
		ID:              runnerID,
		HostID:          m.config.HostID,
		State:           StateBusy,
		InternalIP:      internalIP,
		TapDevice:       tap.Name,
		MAC:             tap.MAC,
		SnapshotVersion: metadata.SnapshotVersion,
		WorkloadKey:     metadata.WorkloadKey,
		Resources: Resources{
			VCPUs:    metadata.VCPUs,
			MemoryMB: metadata.MemoryMB,
		},
		CreatedAt:         metadata.CreatedAt,
		StartedAt:         time.Now(),
		SocketPath:        filepath.Join(m.config.SocketDir, runnerID+".sock"),
		LogPath:           filepath.Join(m.config.LogDir, runnerID+".log"),
		MetricsPath:       filepath.Join(m.config.LogDir, runnerID+".metrics"),
		RootfsOverlay:     overlayPath,
		SessionID:         sessionID,
		SessionDir:        sessionDir,
		SessionLayers:     metadata.Layers,
		TTLSeconds:        metadata.TTLSeconds,
		AutoPause:         metadata.AutoPause,
		ServicePort:       metadata.ServicePort,
		AccessPlaneConfig: metadata.AccessPlaneConfig,
		LastExecAt:        time.Now(),
	}

	m.mu.Lock()
	// Remove any old suspended entry for this runner
	delete(m.runners, runnerID)
	m.runners[runnerID] = runner
	m.vms[runnerID] = vm
	m.uffdHandlers[runnerID] = uffdHandler
	m.mu.Unlock()
	lease.Commit()

	// Inject MMDS data BEFORE resuming so the capsule-thaw-agent sees fresh config
	allocReq := AllocateRequest{
		WorkloadKey: metadata.WorkloadKey,
	}
	mmdsData := m.buildMMDSData(ctx, runner, tap, allocReq)
	if metadata.AccessPlaneConfig != nil {
		mmdsData.Latest.Proxy.Address = metadata.AccessPlaneConfig.ProxyEndpoint
		mmdsData.Latest.Proxy.CACertPEM = metadata.AccessPlaneConfig.CACertPEM
		mmdsData.Latest.Proxy.APIEndpoint = metadata.AccessPlaneConfig.APIEndpoint
		mmdsData.Latest.Proxy.TenantID = metadata.AccessPlaneConfig.TenantID
	}
	if attestationToken != "" {
		mmdsData.Latest.Proxy.AttestationToken = attestationToken
	}
	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		m.logger.WithError(err).Warn("Failed to set MMDS data on resumed runner")
	}

	// Now resume the VM — capsule-thaw-agent will read the fresh MMDS data
	if err := vm.Resume(ctx); err != nil {
		vm.Stop()
		uffdHandler.Stop()
		m.mu.Lock()
		delete(m.runners, runnerID)
		delete(m.vms, runnerID)
		delete(m.uffdHandlers, runnerID)
		m.cleanupRunner(runnerID, tap.Name, "")
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to resume VM after MMDS injection: %w", err)
	}

	// Wait for the capsule-thaw-agent inside the VM to become responsive.
	// After snapshot restore the agent needs to detect the new MMDS
	// current_time, thaw frozen filesystems, and reconfigure networking.
	readyTimeout := 5 * time.Second
	if err := m.waitForThawAgentReady(ctx, internalIP.String(), readyTimeout); err != nil {
		m.logger.WithError(err).WithFields(logrus.Fields{
			"runner_id":  runnerID,
			"session_id": sessionID,
			"ip":         internalIP.String(),
		}).Warn("Thaw-agent not ready after session resume")
	}

	m.logger.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"session_id": sessionID,
		"layers":     metadata.Layers,
	}).Info("Runner resumed from session snapshot successfully")

	return runner, nil
}

// waitForThawAgentReady polls the capsule-thaw-agent /alive endpoint until it
// returns HTTP 200 or the timeout expires.
func (m *Manager) waitForThawAgentReady(ctx context.Context, ip string, timeout time.Duration) error {
	aliveURL := fmt.Sprintf("http://%s:%d/alive", ip, snapshot.ThawAgentDebugPort)
	deadline := time.Now().Add(timeout)
	pollInterval := 10 * time.Millisecond

	client := &http.Client{Timeout: 100 * time.Millisecond}

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		resp, err := client.Get(aliveURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				m.logger.WithField("url", aliveURL).Debug("Thaw-agent ready after session resume")
				return nil
			}
		}

		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("capsule-thaw-agent at %s did not become ready within %v", aliveURL, timeout)
}

// buildExtensionDriveBaseIndex constructs a ChunkIndex for an extension drive
// from the golden CI metadata's ExtensionDrives map. If the drive is not found
// in the metadata (or metadata is nil), an empty base is returned.
func buildExtensionDriveBaseIndex(meta *snapshot.ChunkedSnapshotMetadata, driveID string) *snapshot.ChunkIndex {
	idx := &snapshot.ChunkIndex{
		Version:        "1",
		ChunkSizeBytes: snapshot.DefaultChunkSize,
	}
	idx.CAS.Algo = "sha256"
	idx.CAS.Layout = "chunks/disk/{p0}/{hash}"
	idx.CAS.Kind = "disk"
	idx.Region.Name = driveID
	idx.Region.Coverage = "sparse"
	idx.Region.DefaultFill = "zero"

	if meta == nil {
		return idx
	}
	if extDrive, ok := meta.ExtensionDrives[driveID]; ok {
		idx.ChunkSizeBytes = meta.ChunkSize
		idx.Region.LogicalSizeBytes = extDrive.SizeBytes
		for _, ref := range extDrive.Chunks {
			if ref.Hash == snapshot.ZeroChunkHash {
				continue
			}
			idx.Region.Extents = append(idx.Region.Extents, snapshot.ManifestChunkRef{
				Offset:       ref.Offset,
				Length:       ref.Size,
				Hash:         ref.Hash,
				StoredLength: ref.CompressedSize,
			})
		}
	}
	return idx
}

// buildRootfsDriveBaseIndex constructs a ChunkIndex for the rootfs drive
// from the golden CI metadata. If metadata is nil, an empty base is returned.
func buildRootfsDriveBaseIndex(meta *snapshot.ChunkedSnapshotMetadata) *snapshot.ChunkIndex {
	idx := &snapshot.ChunkIndex{
		Version:        "1",
		ChunkSizeBytes: snapshot.DefaultChunkSize,
	}
	idx.CAS.Algo = "sha256"
	idx.CAS.Layout = "chunks/disk/{p0}/{hash}"
	idx.CAS.Kind = "disk"
	idx.Region.Name = "__rootfs__"
	idx.Region.Coverage = "sparse"
	idx.Region.DefaultFill = "zero"

	if meta == nil {
		return idx
	}

	idx.ChunkSizeBytes = meta.ChunkSize
	idx.Region.LogicalSizeBytes = meta.TotalDiskSize
	for _, ref := range meta.RootfsChunks {
		if ref.Hash == snapshot.ZeroChunkHash {
			continue
		}
		idx.Region.Extents = append(idx.Region.Extents, snapshot.ManifestChunkRef{
			Offset:       ref.Offset,
			Length:       ref.Size,
			Hash:         ref.Hash,
			StoredLength: ref.CompressedSize,
		})
	}

	return idx
}

// IncrementActiveExecs atomically increments the active exec count for a runner.
func (m *Manager) IncrementActiveExecs(runnerID string) {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return
	}
	atomic.AddInt32(&runner.ActiveExecs, 1)
}

// DecrementActiveExecs atomically decrements the active exec count and updates LastExecAt.
// Deprecated: Use ReleaseExec for state-safe release.
func (m *Manager) DecrementActiveExecs(runnerID string) {
	m.ReleaseExec(runnerID)
}

// ResetTTL updates LastExecAt for a runner, extending its idle TTL timer.
func (m *Manager) ResetTTL(runnerID string) {
	m.mu.Lock()
	runner, exists := m.runners[runnerID]
	if exists {
		runner.LastExecAt = time.Now()
	}
	m.mu.Unlock()
}

// SessionExists checks if a session snapshot exists on disk.
func (m *Manager) SessionExists(sessionID string) bool {
	metadataPath := filepath.Join(m.sessionBaseDir(), sessionID, "metadata.json")
	_, err := os.Stat(metadataPath)
	return err == nil
}

// GetSessionMetadata reads the metadata for a session snapshot.
func (m *Manager) GetSessionMetadata(sessionID string) (*SessionMetadata, error) {
	metadataPath := filepath.Join(m.sessionBaseDir(), sessionID, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}
	var meta SessionMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// CleanupSession removes a session's snapshot files from disk.
func (m *Manager) CleanupSession(sessionID string) error {
	sessionDir := filepath.Join(m.sessionBaseDir(), sessionID)
	return os.RemoveAll(sessionDir)
}

// FindRunnerBySessionID returns a runner with the given session ID, if any.
func (m *Manager) FindRunnerBySessionID(sessionID string) *Runner {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.runners {
		if r.SessionID == sessionID {
			return r
		}
	}
	return nil
}
