package runner

import (
	"context"
	"fmt"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

type sessionHead struct {
	ManifestPath string
	Manifest     *snapshot.SnapshotManifest
	Metadata     *SessionMetadata
}

func sessionCheckpointGCSBase(sessionID string, generation int) string {
	return fmt.Sprintf("sessions/%s/checkpoints/%06d", sessionID, generation)
}

func sessionDiskIndexObjectsFromManifest(man *snapshot.SnapshotManifest) map[string]string {
	if man == nil {
		return nil
	}
	out := make(map[string]string)
	if man.Disk.ChunkIndexObject != "" {
		out["__rootfs__"] = man.Disk.ChunkIndexObject
	}
	for driveID, section := range man.ExtensionDisks {
		if section.ChunkIndexObject == "" {
			continue
		}
		out[driveID] = section.ChunkIndexObject
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m *Manager) resolveSessionHead(ctx context.Context, sessionID string, current *Runner) *sessionHead {
	head := &sessionHead{}
	if current != nil && current.SessionManifestPath != "" {
		head.ManifestPath = current.SessionManifestPath
	}
	if meta, err := m.GetSessionMetadata(sessionID); err == nil {
		head.Metadata = meta
		if head.ManifestPath == "" {
			head.ManifestPath = meta.GCSManifestPath
		}
	}
	if head.ManifestPath == "" || m.sessionMemStore == nil {
		return head
	}
	uploader := snapshot.NewSessionChunkUploader(m.sessionMemStore, m.sessionDiskStore, m.logger.Logger)
	man, err := uploader.DownloadManifest(ctx, head.ManifestPath)
	if err != nil {
		m.logger.WithError(err).WithFields(map[string]any{
			"session_id":    sessionID,
			"manifest_path": head.ManifestPath,
		}).Warn("Failed to download previous session head manifest")
		return head
	}
	head.Manifest = man
	return head
}
