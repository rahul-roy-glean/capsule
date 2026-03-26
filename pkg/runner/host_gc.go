package runner

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	gcArtifactSessions     = "sessions"
	gcArtifactSessionState = "session_state"
	gcArtifactChunkCache   = "chunk_cache"
	gcArtifactLogs         = "logs"
	gcArtifactQuarantine   = "quarantine"
)

type GCUsage struct {
	SessionsBytes     int64
	SessionStateBytes int64
	ChunkCacheBytes   int64
	LogBytes          int64
	QuarantineBytes   int64
}

type GCStatTotals struct {
	BytesReclaimed int64
	FilesRemoved   int64
}

type GCMetricsSnapshot struct {
	Sessions     GCStatTotals
	SessionState GCStatTotals
	ChunkCache   GCStatTotals
	Logs         GCStatTotals
	Quarantine   GCStatTotals
}

const (
	// DefaultSessionSweepInterval controls how often the host janitor scans
	// local session artifacts when no override is configured.
	DefaultSessionSweepInterval = 15 * time.Minute
	// DefaultSessionMaxAge is the fallback retention for paused session data
	// when a session does not record an explicit max age.
	DefaultSessionMaxAge = 24 * time.Hour
	// DefaultSessionStateMaxAge is the fallback retention for temporary
	// SocketDir/session-state files left behind by failed resumes.
	DefaultSessionStateMaxAge = 1 * time.Hour
	// DefaultChunkCacheLowWatermark is the target fraction of ChunkCacheMaxBytes
	// retained after pruning once the chunk cache exceeds its cap.
	DefaultChunkCacheLowWatermark = 0.7
	// DefaultLogMaxAge is the retention for inactive runner logs.
	DefaultLogMaxAge = 72 * time.Hour
	// DefaultQuarantineMaxAge is the retention for inactive quarantine data.
	DefaultQuarantineMaxAge = 7 * 24 * time.Hour
)

func (m *Manager) sessionSweepInterval() time.Duration {
	return m.config.SessionSweepInterval
}

func (m *Manager) sessionDefaultMaxAge() time.Duration {
	if m.config.SessionDefaultMaxAge > 0 {
		return m.config.SessionDefaultMaxAge
	}
	return DefaultSessionMaxAge
}

func (m *Manager) sessionStateMaxAge() time.Duration {
	if m.config.SessionStateMaxAge > 0 {
		return m.config.SessionStateMaxAge
	}
	return DefaultSessionStateMaxAge
}

func (m *Manager) chunkCacheMaxBytes() int64 {
	return m.config.ChunkCacheMaxBytes
}

func (m *Manager) chunkCacheLowWatermark() float64 {
	if m.config.ChunkCacheLowWatermark > 0 && m.config.ChunkCacheLowWatermark < 1 {
		return m.config.ChunkCacheLowWatermark
	}
	return DefaultChunkCacheLowWatermark
}

func (m *Manager) logMaxAge() time.Duration {
	if m.config.LogMaxAge > 0 {
		return m.config.LogMaxAge
	}
	return DefaultLogMaxAge
}

func (m *Manager) quarantineMaxAge() time.Duration {
	if m.config.QuarantineMaxAge > 0 {
		return m.config.QuarantineMaxAge
	}
	return DefaultQuarantineMaxAge
}

func (m *Manager) sessionMaxAgeSecondsForRunner(runner *Runner) int {
	if runner != nil && runner.SessionMaxAgeConfigured {
		return runner.SessionMaxAgeSeconds
	}
	if maxAge := m.sessionDefaultMaxAge(); maxAge > 0 {
		return int(maxAge / time.Second)
	}
	return 0
}

func (m *Manager) startArtifactJanitor() {
	interval := m.sessionSweepInterval()
	if interval <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				if cleaned, err := m.CleanupExpiredSessions(now); err != nil {
					m.logger.WithError(err).Warn("Failed to clean up expired sessions")
				} else if cleaned > 0 {
					m.logger.WithField("cleaned", cleaned).Info("Cleaned expired local sessions")
				}
				if cleaned, err := m.CleanupStaleSessionState(now); err != nil {
					m.logger.WithError(err).Warn("Failed to clean up stale session-state files")
				} else if cleaned > 0 {
					m.logger.WithField("cleaned", cleaned).Info("Cleaned stale session-state files")
				}
				if bytesFreed, filesRemoved, err := m.CleanupLocalChunkCache(now); err != nil {
					m.logger.WithError(err).Warn("Failed to prune local chunk cache")
				} else if filesRemoved > 0 {
					m.logger.WithFields(map[string]any{
						"files_removed": filesRemoved,
						"bytes_freed":   bytesFreed,
					}).Info("Pruned local chunk cache")
				}
				if cleaned, err := m.CleanupStaleLogs(now); err != nil {
					m.logger.WithError(err).Warn("Failed to clean up stale runner logs")
				} else if cleaned > 0 {
					m.logger.WithField("cleaned", cleaned).Info("Cleaned stale runner logs")
				}
				if cleaned, err := m.CleanupExpiredQuarantine(now); err != nil {
					m.logger.WithError(err).Warn("Failed to clean up expired quarantine dirs")
				} else if cleaned > 0 {
					m.logger.WithField("cleaned", cleaned).Info("Cleaned expired quarantine dirs")
				}
			case <-m.stopCh:
				return
			}
		}
	}()
}

func (m *Manager) activeArtifactsForGC() (map[string]struct{}, map[string]struct{}) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	activeSessions := make(map[string]struct{}, len(m.runners)+len(m.pendingSessions))
	activeRunnerIDs := make(map[string]struct{}, len(m.runners)+len(m.pendingSessions))
	for runnerID, runner := range m.runners {
		activeRunnerIDs[runnerID] = struct{}{}
		if runner.SessionID != "" {
			activeSessions[runner.SessionID] = struct{}{}
		}
	}
	for sessionID, runnerID := range m.pendingSessions {
		activeSessions[sessionID] = struct{}{}
		if runnerID != "" {
			activeRunnerIDs[runnerID] = struct{}{}
		}
	}
	return activeSessions, activeRunnerIDs
}

// CleanupExpiredSessions removes old local session directories that are no longer
// associated with an active or pending runner/session.
func (m *Manager) CleanupExpiredSessions(now time.Time) (int, error) {
	sessionDir := m.sessionBaseDir()
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	activeSessions, _ := m.activeArtifactsForGC()
	defaultMaxAge := m.sessionDefaultMaxAge()
	cleaned := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionID := entry.Name()
		if _, exists := activeSessions[sessionID]; exists {
			continue
		}

		sessionPath := filepath.Join(sessionDir, sessionID)
		meta, metaErr := m.GetSessionMetadata(sessionID)
		if metaErr != nil {
			info, infoErr := entry.Info()
			if infoErr != nil {
				m.logger.WithError(infoErr).WithField("session_id", sessionID).Warn("Failed to stat local session dir")
				continue
			}
			if defaultMaxAge <= 0 || now.Sub(info.ModTime()) < defaultMaxAge {
				continue
			}
			bytesFreed := dirSize(sessionPath)
			if removeErr := os.RemoveAll(sessionPath); removeErr != nil {
				m.logger.WithError(removeErr).WithField("session_id", sessionID).Warn("Failed to remove stale session dir without metadata")
				continue
			}
			m.recordGCReclaim(gcArtifactSessions, bytesFreed, 1)
			cleaned++
			continue
		}

		maxAge := defaultMaxAge
		if meta.SessionMaxAgeConfigured {
			maxAge = time.Duration(meta.SessionMaxAgeSeconds) * time.Second
		} else if meta.SessionMaxAgeSeconds > 0 {
			maxAge = time.Duration(meta.SessionMaxAgeSeconds) * time.Second
		}
		if maxAge <= 0 {
			continue
		}

		refTime := meta.PausedAt
		if refTime.IsZero() {
			refTime = meta.CreatedAt
		}
		if refTime.IsZero() {
			info, infoErr := entry.Info()
			if infoErr != nil {
				m.logger.WithError(infoErr).WithField("session_id", sessionID).Warn("Failed to stat local session dir")
				continue
			}
			refTime = info.ModTime()
		}
		if now.Sub(refTime) < maxAge {
			continue
		}

		bytesFreed := dirSize(sessionPath)
		if removeErr := os.RemoveAll(sessionPath); removeErr != nil {
			m.logger.WithError(removeErr).WithField("session_id", sessionID).Warn("Failed to remove expired session dir")
			continue
		}
		m.recordGCReclaim(gcArtifactSessions, bytesFreed, 1)
		cleaned++
	}
	return cleaned, nil
}

type chunkCacheFile struct {
	path    string
	size    int64
	modTime time.Time
}

func (m *Manager) chunkCacheDir() string {
	if m.config.SnapshotCachePath == "" {
		return ""
	}
	return filepath.Join(m.config.SnapshotCachePath, "chunks")
}

// CleanupLocalChunkCache prunes the on-disk chunk cache when it exceeds the
// configured size cap. Oldest files are removed first until the low watermark
// target is reached.
func (m *Manager) CleanupLocalChunkCache(now time.Time) (int64, int, error) {
	maxBytes := m.chunkCacheMaxBytes()
	if maxBytes <= 0 {
		return 0, 0, nil
	}

	chunkDir := m.chunkCacheDir()
	if chunkDir == "" {
		return 0, 0, nil
	}

	var (
		totalSize int64
		files     []chunkCacheFile
	)
	err := filepath.Walk(chunkDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".chunk-") {
			return nil
		}
		totalSize += info.Size()
		files = append(files, chunkCacheFile{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	if totalSize <= maxBytes {
		return 0, 0, nil
	}

	targetBytes := int64(float64(maxBytes) * m.chunkCacheLowWatermark())
	if targetBytes < 0 {
		targetBytes = 0
	}
	if targetBytes >= maxBytes {
		targetBytes = maxBytes - 1
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.Before(files[j].modTime)
	})

	var bytesFreed int64
	filesRemoved := 0
	for _, file := range files {
		if totalSize <= targetBytes {
			break
		}
		if err := os.Remove(file.path); err != nil {
			if os.IsNotExist(err) {
				totalSize -= file.size
				continue
			}
			m.logger.WithError(err).WithField("file", file.path).Warn("Failed to remove chunk cache file")
			continue
		}
		totalSize -= file.size
		bytesFreed += file.size
		filesRemoved++
		m.cleanupEmptyChunkDirs(chunkDir, filepath.Dir(file.path))
	}
	if filesRemoved > 0 {
		m.recordGCReclaim(gcArtifactChunkCache, bytesFreed, int64(filesRemoved))
	}
	return bytesFreed, filesRemoved, nil
}

func (m *Manager) cleanupEmptyChunkDirs(rootDir, dir string) {
	for dir != "" && dir != rootDir && dir != filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// CleanupStaleLogs removes inactive runner log, console, and metrics files once
// they have aged past the configured retention.
func (m *Manager) CleanupStaleLogs(now time.Time) (int, error) {
	if m.config.LogDir == "" {
		return 0, nil
	}
	maxAge := m.logMaxAge()
	if maxAge <= 0 {
		return 0, nil
	}

	entries, err := os.ReadDir(m.config.LogDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	_, activeRunnerIDs := m.activeArtifactsForGC()
	cleaned := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		runnerID, ok := runnerIDFromArtifactName(entry.Name())
		if !ok {
			continue
		}
		if _, exists := activeRunnerIDs[runnerID]; exists {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			m.logger.WithError(infoErr).WithField("file", entry.Name()).Warn("Failed to stat runner log artifact")
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue
		}
		logPath := filepath.Join(m.config.LogDir, entry.Name())
		if removeErr := os.Remove(logPath); removeErr != nil && !os.IsNotExist(removeErr) {
			m.logger.WithError(removeErr).WithField("file", logPath).Warn("Failed to remove stale runner log artifact")
			continue
		}
		m.recordGCReclaim(gcArtifactLogs, info.Size(), 1)
		cleaned++
	}
	return cleaned, nil
}

// CleanupExpiredQuarantine removes inactive quarantine directories once they age
// past the configured retention.
func (m *Manager) CleanupExpiredQuarantine(now time.Time) (int, error) {
	if m.config.QuarantineDir == "" {
		return 0, nil
	}
	maxAge := m.quarantineMaxAge()
	if maxAge <= 0 {
		return 0, nil
	}

	entries, err := os.ReadDir(m.config.QuarantineDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	_, activeRunnerIDs := m.activeArtifactsForGC()
	cleaned := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runnerID := entry.Name()
		if _, exists := activeRunnerIDs[runnerID]; exists {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			m.logger.WithError(infoErr).WithField("runner_id", runnerID).Warn("Failed to stat quarantine dir")
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue
		}
		dirPath := filepath.Join(m.config.QuarantineDir, runnerID)
		bytesFreed := dirSize(dirPath)
		if removeErr := os.RemoveAll(dirPath); removeErr != nil {
			m.logger.WithError(removeErr).WithField("runner_id", runnerID).Warn("Failed to remove expired quarantine dir")
			continue
		}
		m.recordGCReclaim(gcArtifactQuarantine, bytesFreed, 1)
		cleaned++
	}
	return cleaned, nil
}

func runnerIDFromArtifactName(name string) (string, bool) {
	switch {
	case strings.HasSuffix(name, ".console.log"):
		runnerID := strings.TrimSuffix(name, ".console.log")
		return runnerID, runnerID != ""
	case strings.HasSuffix(name, ".metrics"):
		runnerID := strings.TrimSuffix(name, ".metrics")
		return runnerID, runnerID != ""
	case strings.HasSuffix(name, ".log"):
		runnerID := strings.TrimSuffix(name, ".log")
		return runnerID, runnerID != ""
	default:
		return "", false
	}
}

func (m *Manager) recordGCReclaim(artifactClass string, bytesFreed, filesRemoved int64) {
	stats := m.gcArtifactStats(artifactClass)
	if stats == nil {
		return
	}
	if bytesFreed > 0 {
		stats.bytesReclaimed.Add(bytesFreed)
	}
	if filesRemoved > 0 {
		stats.filesRemoved.Add(filesRemoved)
	}
}

func (m *Manager) gcArtifactStats(artifactClass string) *gcArtifactStats {
	switch artifactClass {
	case gcArtifactSessions:
		return &m.gcStats.sessions
	case gcArtifactSessionState:
		return &m.gcStats.sessionState
	case gcArtifactChunkCache:
		return &m.gcStats.chunkCache
	case gcArtifactLogs:
		return &m.gcStats.logs
	case gcArtifactQuarantine:
		return &m.gcStats.quarantine
	default:
		return nil
	}
}

func (m *Manager) GCMetricsSnapshot() GCMetricsSnapshot {
	return GCMetricsSnapshot{
		Sessions: GCStatTotals{
			BytesReclaimed: m.gcStats.sessions.bytesReclaimed.Load(),
			FilesRemoved:   m.gcStats.sessions.filesRemoved.Load(),
		},
		SessionState: GCStatTotals{
			BytesReclaimed: m.gcStats.sessionState.bytesReclaimed.Load(),
			FilesRemoved:   m.gcStats.sessionState.filesRemoved.Load(),
		},
		ChunkCache: GCStatTotals{
			BytesReclaimed: m.gcStats.chunkCache.bytesReclaimed.Load(),
			FilesRemoved:   m.gcStats.chunkCache.filesRemoved.Load(),
		},
		Logs: GCStatTotals{
			BytesReclaimed: m.gcStats.logs.bytesReclaimed.Load(),
			FilesRemoved:   m.gcStats.logs.filesRemoved.Load(),
		},
		Quarantine: GCStatTotals{
			BytesReclaimed: m.gcStats.quarantine.bytesReclaimed.Load(),
			FilesRemoved:   m.gcStats.quarantine.filesRemoved.Load(),
		},
	}
}

func (m *Manager) CollectGCUsage() GCUsage {
	return GCUsage{
		SessionsBytes:     dirSize(m.sessionBaseDir()),
		SessionStateBytes: dirSize(filepath.Join(m.config.SocketDir, "session-state")),
		ChunkCacheBytes:   dirSize(m.chunkCacheDir()),
		LogBytes:          dirSize(m.config.LogDir),
		QuarantineBytes:   dirSize(m.config.QuarantineDir),
	}
}

func dirSize(root string) int64 {
	if root == "" {
		return 0
	}
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// CleanupStaleSessionState removes temporary *.state files left behind in
// SocketDir/session-state by interrupted or failed resume attempts.
func (m *Manager) CleanupStaleSessionState(now time.Time) (int, error) {
	stateDir := filepath.Join(m.config.SocketDir, "session-state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	_, activeRunnerIDs := m.activeArtifactsForGC()
	maxAge := m.sessionStateMaxAge()
	if maxAge <= 0 {
		return 0, nil
	}

	cleaned := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".state") {
			continue
		}
		runnerID := strings.TrimSuffix(entry.Name(), ".state")
		if _, exists := activeRunnerIDs[runnerID]; exists {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			m.logger.WithError(infoErr).WithField("file", entry.Name()).Warn("Failed to stat session-state file")
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue
		}

		statePath := filepath.Join(stateDir, entry.Name())
		if removeErr := os.Remove(statePath); removeErr != nil && !os.IsNotExist(removeErr) {
			m.logger.WithError(removeErr).WithField("file", statePath).Warn("Failed to remove stale session-state file")
			continue
		}
		m.recordGCReclaim(gcArtifactSessionState, info.Size(), 1)
		cleaned++
	}
	return cleaned, nil
}
