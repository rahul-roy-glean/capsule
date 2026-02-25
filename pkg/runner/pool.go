package runner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// RunnerKey identifies runners that can be reused for similar tasks.
// Runners with matching keys are candidates for pool reuse.
type RunnerKey struct {
	SnapshotVersion string            // Snapshot version for binary compatibility
	Platform        string            // OS/Arch (e.g., "linux/amd64")
	GitHubRepo      string            // For pre-cloned repo matching
	Labels          map[string]string // Custom matching labels
}

// PoolConfig configures the runner pool
type PoolConfig struct {
	Enabled              bool
	MaxPooledRunners     int           // Max paused runners (0 = derive from resources)
	MaxTotalMemoryBytes  int64         // Total memory limit for pooled runners
	MaxRunnerMemoryBytes int64         // Per-runner memory limit (default 2GB)
	MaxRunnerDiskBytes   int64         // Per-runner disk limit (default 16GB)
	RecycleTimeout       time.Duration // Timeout for pause operation
	MaxUnpauseAttempts   int           // Retry attempts on unpause failure (default 5)
}

// DefaultPoolConfig returns a PoolConfig with sensible defaults
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		Enabled:              false,
		MaxPooledRunners:     10,
		MaxTotalMemoryBytes:  20 * 1024 * 1024 * 1024, // 20GB
		MaxRunnerMemoryBytes: 2 * 1024 * 1024 * 1024,  // 2GB
		MaxRunnerDiskBytes:   16 * 1024 * 1024 * 1024, // 16GB
		RecycleTimeout:       30 * time.Second,
		MaxUnpauseAttempts:   5,
	}
}

// pooledRunner wraps Runner with pool metadata
type pooledRunner struct {
	*Runner
	key              *RunnerKey
	memoryUsageBytes int64
	diskUsageBytes   int64
	pausedAt         time.Time
}

// ReleaseOptions controls runner release behavior
type ReleaseOptions struct {
	Destroy         bool // Force destroy (don't recycle)
	TryRecycle      bool // Attempt to recycle to pool
	FinishedCleanly bool // Task completed without error
}

// PoolStats holds pool statistics for monitoring
type PoolStats struct {
	PooledRunners    int   `json:"pooled_runners"`
	MaxRunners       int   `json:"max_runners"`
	MemoryUsageBytes int64 `json:"memory_usage_bytes"`
	MaxMemoryBytes   int64 `json:"max_memory_bytes"`
	PoolHits         int64 `json:"pool_hits"`
	PoolMisses       int64 `json:"pool_misses"`
	Evictions        int64 `json:"evictions"`
	RecycleFailures  int64 `json:"recycle_failures"`
}

// Pool manages reusable runners with LRU eviction.
// When a task completes, instead of destroying the VM, pause it and add it to the pool.
// The next compatible task can resume that VM instantly (~10ms) instead of creating a new one.
type Pool struct {
	mu              sync.RWMutex
	runners         []*pooledRunner // All runners in LRU order (oldest first)
	config          PoolConfig
	isShuttingDown  bool
	pendingRemovals sync.WaitGroup
	logger          *logrus.Entry

	// Callbacks for VM operations (set by Manager)
	pauseVM    func(ctx context.Context, runnerID string) error
	resumeVM   func(ctx context.Context, runnerID string) error
	getVMStats func(ctx context.Context, runnerID string) (*VMStats, error)
	removeVM   func(ctx context.Context, runnerID string) error

	// Stats (atomic for lock-free reads)
	poolHits        int64
	poolMisses      int64
	evictions       int64
	recycleFailures int64
}

// VMStats holds VM resource statistics
type VMStats struct {
	MemoryUsageBytes int64
	DiskUsageBytes   int64
}

// NewPool creates a new runner pool
func NewPool(cfg PoolConfig, logger *logrus.Logger) *Pool {
	if logger == nil {
		logger = logrus.New()
	}

	// Apply defaults for unset values
	if cfg.MaxUnpauseAttempts == 0 {
		cfg.MaxUnpauseAttempts = 5
	}
	if cfg.RecycleTimeout == 0 {
		cfg.RecycleTimeout = 30 * time.Second
	}
	if cfg.MaxRunnerMemoryBytes == 0 {
		cfg.MaxRunnerMemoryBytes = 2 * 1024 * 1024 * 1024 // 2GB
	}
	if cfg.MaxRunnerDiskBytes == 0 {
		cfg.MaxRunnerDiskBytes = 16 * 1024 * 1024 * 1024 // 16GB
	}

	return &Pool{
		runners: make([]*pooledRunner, 0),
		config:  cfg,
		logger:  logger.WithField("component", "runner-pool"),
	}
}

// SetCallbacks sets the VM operation callbacks. Must be called before using the pool.
func (p *Pool) SetCallbacks(
	pauseVM func(ctx context.Context, runnerID string) error,
	resumeVM func(ctx context.Context, runnerID string) error,
	getVMStats func(ctx context.Context, runnerID string) (*VMStats, error),
	removeVM func(ctx context.Context, runnerID string) error,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pauseVM = pauseVM
	p.resumeVM = resumeVM
	p.getVMStats = getVMStats
	p.removeVM = removeVM
}

// Get attempts to take a runner from the pool matching the key.
// Returns nil if no match found (caller should create new runner).
func (p *Pool) Get(ctx context.Context, key *RunnerKey) *pooledRunner {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isShuttingDown {
		return nil
	}

	// Search backwards (most recently paused = most likely to succeed)
	for i := len(p.runners) - 1; i >= 0; i-- {
		r := p.runners[i]
		if r.Runner.State != StatePaused {
			continue
		}
		if !p.keysMatch(key, r.key) {
			continue
		}

		// Try to unpause with retries
		var lastErr error
		for attempt := 0; attempt < p.config.MaxUnpauseAttempts; attempt++ {
			if p.resumeVM == nil {
				lastErr = fmt.Errorf("resumeVM callback not set")
				break
			}

			if err := p.resumeVM(ctx, r.Runner.ID); err == nil {
				// Success - remove from pool and return
				p.runners = append(p.runners[:i], p.runners[i+1:]...)
				r.Runner.State = StateIdle
				atomic.AddInt64(&p.poolHits, 1)

				p.logger.WithFields(logrus.Fields{
					"runner_id": r.Runner.ID,
					"attempts":  attempt + 1,
				}).Debug("Runner resumed from pool")

				return r
			} else {
				lastErr = err
				p.logger.WithError(err).WithFields(logrus.Fields{
					"runner_id": r.Runner.ID,
					"attempt":   attempt + 1,
				}).Debug("Failed to resume runner, retrying")
			}
		}

		// Unpause failed - remove bad runner and continue search
		p.logger.WithError(lastErr).WithField("runner_id", r.Runner.ID).Warn("Failed to resume runner after all attempts, removing from pool")
		p.runners = append(p.runners[:i], p.runners[i+1:]...)
		if p.removeVM != nil {
			p.pendingRemovals.Add(1)
			go func(runnerID string) {
				defer p.pendingRemovals.Done()
				removeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := p.removeVM(removeCtx, runnerID); err != nil {
					p.logger.WithError(err).WithField("runner_id", runnerID).Warn("Failed to remove bad runner")
				}
			}(r.Runner.ID)
		}
	}

	atomic.AddInt64(&p.poolMisses, 1)
	return nil // No match found
}

// Add pauses the runner and adds it to the pool
func (p *Pool) Add(ctx context.Context, r *pooledRunner) error {
	p.mu.Lock()

	if p.isShuttingDown {
		p.mu.Unlock()
		return fmt.Errorf("pool is shutting down")
	}
	p.mu.Unlock()

	// Pause the VM (do this without holding the lock to avoid blocking)
	if p.pauseVM == nil {
		return fmt.Errorf("pauseVM callback not set")
	}

	pauseCtx, cancel := context.WithTimeout(ctx, p.config.RecycleTimeout)
	defer cancel()

	if err := p.pauseVM(pauseCtx, r.Runner.ID); err != nil {
		return fmt.Errorf("pause failed: %w", err)
	}

	r.Runner.State = StatePaused
	r.pausedAt = time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check we're not shutting down after the pause
	if p.isShuttingDown {
		// Resume the VM since we can't add it
		if p.resumeVM != nil {
			p.resumeVM(context.Background(), r.Runner.ID)
		}
		return fmt.Errorf("pool is shutting down")
	}

	// Check resource limits and evict if needed (while holding lock)
	for p.shouldEvict(r) {
		if err := p.evictOldestLocked(ctx); err != nil {
			return fmt.Errorf("eviction failed: %w", err)
		}
	}

	p.runners = append(p.runners, r)

	p.logger.WithFields(logrus.Fields{
		"runner_id":   r.Runner.ID,
		"pool_size":   len(p.runners),
		"memory_used": p.currentMemoryUsageLocked(),
	}).Debug("Runner added to pool")

	return nil
}

// TryRecycle attempts to recycle a runner after task completion
func (p *Pool) TryRecycle(ctx context.Context, r *pooledRunner, finishedCleanly bool) error {
	if !finishedCleanly {
		atomic.AddInt64(&p.recycleFailures, 1)
		return fmt.Errorf("task did not finish cleanly")
	}

	// Get memory/disk usage if callback available
	if p.getVMStats != nil {
		stats, err := p.getVMStats(ctx, r.Runner.ID)
		if err != nil {
			p.logger.WithError(err).WithField("runner_id", r.Runner.ID).Debug("Failed to get VM stats")
		} else {
			r.memoryUsageBytes = stats.MemoryUsageBytes
			r.diskUsageBytes = stats.DiskUsageBytes
		}
	}

	// Check if runner exceeds per-runner limits
	if r.memoryUsageBytes > p.config.MaxRunnerMemoryBytes {
		atomic.AddInt64(&p.recycleFailures, 1)
		return fmt.Errorf("runner memory usage %d exceeds limit %d", r.memoryUsageBytes, p.config.MaxRunnerMemoryBytes)
	}
	if r.diskUsageBytes > p.config.MaxRunnerDiskBytes {
		atomic.AddInt64(&p.recycleFailures, 1)
		return fmt.Errorf("runner disk usage %d exceeds limit %d", r.diskUsageBytes, p.config.MaxRunnerDiskBytes)
	}

	if err := p.Add(ctx, r); err != nil {
		atomic.AddInt64(&p.recycleFailures, 1)
		return err
	}

	return nil
}

// shouldEvict returns true if we need to evict runners to make room
func (p *Pool) shouldEvict(newRunner *pooledRunner) bool {
	if len(p.runners) == 0 {
		return false
	}

	// Check runner count limit
	if p.config.MaxPooledRunners > 0 && len(p.runners) >= p.config.MaxPooledRunners {
		return true
	}

	// Check memory limit
	if p.config.MaxTotalMemoryBytes > 0 {
		currentMem := p.currentMemoryUsageLocked()
		if currentMem+newRunner.memoryUsageBytes > p.config.MaxTotalMemoryBytes {
			return true
		}
	}

	return false
}

// evictOldestLocked removes the least recently paused runner.
// Must be called with mutex held.
func (p *Pool) evictOldestLocked(ctx context.Context) error {
	if len(p.runners) == 0 {
		return fmt.Errorf("no runners to evict")
	}

	// Oldest is at the front (LRU order)
	oldest := p.runners[0]
	p.runners = p.runners[1:]

	atomic.AddInt64(&p.evictions, 1)

	p.logger.WithField("runner_id", oldest.Runner.ID).Debug("Evicting oldest runner from pool")

	if p.removeVM != nil {
		p.pendingRemovals.Add(1)
		go func(runnerID string) {
			defer p.pendingRemovals.Done()
			removeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := p.removeVM(removeCtx, runnerID); err != nil {
				p.logger.WithError(err).WithField("runner_id", runnerID).Warn("Failed to remove evicted runner")
			}
		}(oldest.Runner.ID)
	}

	return nil
}

// currentMemoryUsageLocked returns total memory used by pooled runners.
// Must be called with mutex held.
func (p *Pool) currentMemoryUsageLocked() int64 {
	var total int64
	for _, r := range p.runners {
		total += r.memoryUsageBytes
	}
	return total
}

// keysMatch returns true if two runner keys are compatible for reuse
func (p *Pool) keysMatch(a, b *RunnerKey) bool {
	if a == nil || b == nil {
		return a == b
	}

	// Snapshot version must match exactly for binary compatibility
	if a.SnapshotVersion != b.SnapshotVersion {
		return false
	}

	// Platform must match
	if a.Platform != "" && b.Platform != "" && a.Platform != b.Platform {
		return false
	}

	// GitHubRepo matching (empty matches anything)
	if a.GitHubRepo != "" && b.GitHubRepo != "" && a.GitHubRepo != b.GitHubRepo {
		return false
	}

	// Labels: all labels in the request must be present in the pooled runner
	// (pooled runner may have extra labels, that's fine)
	if len(a.Labels) > 0 && len(b.Labels) > 0 {
		for k, v := range a.Labels {
			if bv, ok := b.Labels[k]; !ok || bv != v {
				return false
			}
		}
	}

	return true
}

// Stats returns pool statistics for monitoring
func (p *Pool) Stats() PoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return PoolStats{
		PooledRunners:    len(p.runners),
		MaxRunners:       p.config.MaxPooledRunners,
		MemoryUsageBytes: p.currentMemoryUsageLocked(),
		MaxMemoryBytes:   p.config.MaxTotalMemoryBytes,
		PoolHits:         atomic.LoadInt64(&p.poolHits),
		PoolMisses:       atomic.LoadInt64(&p.poolMisses),
		Evictions:        atomic.LoadInt64(&p.evictions),
		RecycleFailures:  atomic.LoadInt64(&p.recycleFailures),
	}
}

// Len returns the number of pooled runners
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.runners)
}

// Shutdown gracefully shuts down the pool
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.isShuttingDown = true
	runners := make([]*pooledRunner, len(p.runners))
	copy(runners, p.runners)
	p.runners = nil
	p.mu.Unlock()

	p.logger.WithField("pooled_runners", len(runners)).Info("Shutting down runner pool")

	// Remove all pooled runners
	for _, r := range runners {
		if p.removeVM != nil {
			p.pendingRemovals.Add(1)
			go func(runnerID string) {
				defer p.pendingRemovals.Done()
				removeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				if err := p.removeVM(removeCtx, runnerID); err != nil {
					p.logger.WithError(err).WithField("runner_id", runnerID).Warn("Failed to remove runner during shutdown")
				}
			}(r.Runner.ID)
		}
	}

	// Wait for all removals to complete
	done := make(chan struct{})
	go func() {
		p.pendingRemovals.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Info("Runner pool shutdown complete")
		return nil
	case <-ctx.Done():
		p.logger.Warn("Runner pool shutdown timed out")
		return ctx.Err()
	}
}

// Remove removes a specific runner from the pool by ID
func (p *Pool) Remove(runnerID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, r := range p.runners {
		if r.Runner.ID == runnerID {
			p.runners = append(p.runners[:i], p.runners[i+1:]...)
			return true
		}
	}
	return false
}

// FlushOlderThan evicts all pooled runners whose snapshot version doesn't
// match any of the desired versions. desiredVersions maps chunk_key to the
// target version. Runners whose chunk_key/version combo is not in the map are evicted.
// If called with a simple version string, it evicts runners not matching that version.
func (p *Pool) FlushOlderThan(ctx context.Context, version string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	if version == "" {
		return 0
	}

	var toEvict []*pooledRunner
	var remaining []*pooledRunner

	for _, r := range p.runners {
		if r.key != nil && r.key.SnapshotVersion != version {
			toEvict = append(toEvict, r)
		} else {
			remaining = append(remaining, r)
		}
	}

	p.runners = remaining

	for _, r := range toEvict {
		atomic.AddInt64(&p.evictions, 1)
		if p.removeVM != nil {
			p.pendingRemovals.Add(1)
			go func(runnerID string) {
				defer p.pendingRemovals.Done()
				removeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := p.removeVM(removeCtx, runnerID); err != nil {
					p.logger.WithError(err).WithField("runner_id", runnerID).Warn("Failed to remove evicted runner")
				}
			}(r.Runner.ID)
		}
	}

	if len(toEvict) > 0 {
		p.logger.WithFields(logrus.Fields{
			"evicted":         len(toEvict),
			"desired_version": version,
		}).Info("Flushed pooled runners with stale versions")
	}

	return len(toEvict)
}

// FlushByDesiredVersions evicts pooled runners whose snapshot version doesn't
// match the desired version for their chunk key. desiredVersions maps chunk_key → version.
func (p *Pool) FlushByDesiredVersions(ctx context.Context, desiredVersions map[string]string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(desiredVersions) == 0 {
		return 0
	}

	var toEvict []*pooledRunner
	var remaining []*pooledRunner

	for _, r := range p.runners {
		if r.key == nil {
			remaining = append(remaining, r)
			continue
		}
		desired, exists := desiredVersions[r.Runner.GitHubRepo]
		if exists && r.key.SnapshotVersion != desired {
			toEvict = append(toEvict, r)
		} else {
			remaining = append(remaining, r)
		}
	}

	p.runners = remaining

	for _, r := range toEvict {
		atomic.AddInt64(&p.evictions, 1)
		if p.removeVM != nil {
			p.pendingRemovals.Add(1)
			go func(runnerID string) {
				defer p.pendingRemovals.Done()
				removeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := p.removeVM(removeCtx, runnerID); err != nil {
					p.logger.WithError(err).WithField("runner_id", runnerID).Warn("Failed to remove evicted runner")
				}
			}(r.Runner.ID)
		}
	}

	if len(toEvict) > 0 {
		p.logger.WithField("evicted", len(toEvict)).Info("Flushed pooled runners with stale versions")
	}

	return len(toEvict)
}

// GetRunner returns a pooled runner by ID, or nil if not found
func (p *Pool) GetRunner(runnerID string) *pooledRunner {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, r := range p.runners {
		if r.Runner.ID == runnerID {
			return r
		}
	}
	return nil
}
