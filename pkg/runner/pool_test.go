package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestNewPool(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	pool := NewPool(DefaultPoolConfig(), logger)
	if pool == nil {
		t.Fatal("NewPool returned nil")
	}

	stats := pool.Stats()
	if stats.PooledRunners != 0 {
		t.Errorf("Expected 0 pooled runners, got %d", stats.PooledRunners)
	}
}

func TestPoolAdd(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxPooledRunners = 5

	pool := NewPool(cfg, logger)

	// Set up mock callbacks
	pauseCalled := false
	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error {
			pauseCalled = true
			return nil
		},
		func(ctx context.Context, runnerID string) error { return nil },
		nil,
		nil,
	)

	runner := &Runner{
		ID:              "test-runner-1",
		State:           StateIdle,
		SnapshotVersion: "v1",
	}
	pooled := &pooledRunner{
		Runner: runner,
		key: &RunnerKey{
			SnapshotVersion: "v1",
			Platform:        "linux/amd64",
		},
		memoryUsageBytes: 1024 * 1024 * 1024, // 1GB
	}

	ctx := context.Background()
	err := pool.Add(ctx, pooled)
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	if !pauseCalled {
		t.Error("pauseVM callback was not called")
	}

	if pool.Len() != 1 {
		t.Errorf("Expected 1 pooled runner, got %d", pool.Len())
	}

	if runner.State != StatePaused {
		t.Errorf("Expected runner state to be StatePaused, got %s", runner.State)
	}
}

func TestPoolGet(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxPooledRunners = 5

	pool := NewPool(cfg, logger)

	// Set up mock callbacks
	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error { return nil },
		func(ctx context.Context, runnerID string) error { return nil }, // resumeVM
		nil,
		nil,
	)

	// Add a runner directly (bypassing pause since we're testing Get)
	runner := &Runner{
		ID:              "test-runner-1",
		State:           StatePaused,
		SnapshotVersion: "v1",
	}
	pooled := &pooledRunner{
		Runner: runner,
		key: &RunnerKey{
			SnapshotVersion: "v1",
			Platform:        "linux/amd64",
		},
	}
	pool.mu.Lock()
	pool.runners = append(pool.runners, pooled)
	pool.mu.Unlock()

	ctx := context.Background()

	// Get with matching key
	matchingKey := &RunnerKey{
		SnapshotVersion: "v1",
		Platform:        "linux/amd64",
	}
	result := pool.Get(ctx, matchingKey)
	if result == nil {
		t.Fatal("Get returned nil for matching key")
	}
	if result.Runner.ID != "test-runner-1" {
		t.Errorf("Expected runner ID test-runner-1, got %s", result.Runner.ID)
	}
	if result.Runner.State != StateIdle {
		t.Errorf("Expected runner state to be StateIdle after resume, got %s", result.Runner.State)
	}

	// Pool should be empty now
	if pool.Len() != 0 {
		t.Errorf("Expected 0 pooled runners after Get, got %d", pool.Len())
	}

	// Verify pool hit was recorded
	stats := pool.Stats()
	if stats.PoolHits != 1 {
		t.Errorf("Expected 1 pool hit, got %d", stats.PoolHits)
	}
}

func TestPoolGetNoMatch(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxPooledRunners = 5

	pool := NewPool(cfg, logger)

	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error { return nil },
		func(ctx context.Context, runnerID string) error { return nil },
		nil,
		nil,
	)

	// Add a runner with v1 snapshot
	runner := &Runner{
		ID:              "test-runner-1",
		State:           StatePaused,
		SnapshotVersion: "v1",
	}
	pooled := &pooledRunner{
		Runner: runner,
		key: &RunnerKey{
			SnapshotVersion: "v1",
			Platform:        "linux/amd64",
		},
	}
	pool.mu.Lock()
	pool.runners = append(pool.runners, pooled)
	pool.mu.Unlock()

	ctx := context.Background()

	// Try to get with different snapshot version
	nonMatchingKey := &RunnerKey{
		SnapshotVersion: "v2", // Different version
		Platform:        "linux/amd64",
	}
	result := pool.Get(ctx, nonMatchingKey)
	if result != nil {
		t.Error("Get should return nil for non-matching key")
	}

	// Pool should still have the runner
	if pool.Len() != 1 {
		t.Errorf("Expected 1 pooled runner, got %d", pool.Len())
	}

	// Verify pool miss was recorded
	stats := pool.Stats()
	if stats.PoolMisses != 1 {
		t.Errorf("Expected 1 pool miss, got %d", stats.PoolMisses)
	}
}

func TestPoolEviction(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxPooledRunners = 2 // Only allow 2 runners

	pool := NewPool(cfg, logger)

	removedRunners := make([]string, 0)
	var removeMu sync.Mutex

	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error { return nil },
		func(ctx context.Context, runnerID string) error { return nil },
		nil,
		func(ctx context.Context, runnerID string) error {
			removeMu.Lock()
			removedRunners = append(removedRunners, runnerID)
			removeMu.Unlock()
			return nil
		},
	)

	ctx := context.Background()

	// Add 3 runners, which should evict the first one
	for i := 1; i <= 3; i++ {
		runner := &Runner{
			ID:              "test-runner-" + string(rune('0'+i)),
			State:           StateIdle,
			SnapshotVersion: "v1",
		}
		pooled := &pooledRunner{
			Runner: runner,
			key: &RunnerKey{
				SnapshotVersion: "v1",
				Platform:        "linux/amd64",
			},
			memoryUsageBytes: 1024 * 1024 * 1024, // 1GB
		}
		if err := pool.Add(ctx, pooled); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	// Wait for async eviction
	time.Sleep(100 * time.Millisecond)

	// Pool should have 2 runners (max capacity)
	if pool.Len() != 2 {
		t.Errorf("Expected 2 pooled runners, got %d", pool.Len())
	}

	// Verify eviction was recorded
	stats := pool.Stats()
	if stats.Evictions != 1 {
		t.Errorf("Expected 1 eviction, got %d", stats.Evictions)
	}
}

func TestPoolMemoryLimit(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxPooledRunners = 10
	cfg.MaxTotalMemoryBytes = 2 * 1024 * 1024 * 1024 // 2GB total

	pool := NewPool(cfg, logger)

	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error { return nil },
		func(ctx context.Context, runnerID string) error { return nil },
		nil,
		func(ctx context.Context, runnerID string) error { return nil },
	)

	ctx := context.Background()

	// Add 3 runners at 1GB each, which should evict one to stay under 2GB limit
	for i := 1; i <= 3; i++ {
		runner := &Runner{
			ID:              "test-runner-" + string(rune('0'+i)),
			State:           StateIdle,
			SnapshotVersion: "v1",
		}
		pooled := &pooledRunner{
			Runner: runner,
			key: &RunnerKey{
				SnapshotVersion: "v1",
				Platform:        "linux/amd64",
			},
			memoryUsageBytes: 1024 * 1024 * 1024, // 1GB each
		}
		if err := pool.Add(ctx, pooled); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	// Wait for async eviction
	time.Sleep(100 * time.Millisecond)

	// Pool should have 2 runners (2GB limit with 1GB each)
	if pool.Len() != 2 {
		t.Errorf("Expected 2 pooled runners, got %d", pool.Len())
	}
}

func TestPoolShutdown(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true

	pool := NewPool(cfg, logger)

	removedCount := 0
	var removeMu sync.Mutex

	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error { return nil },
		func(ctx context.Context, runnerID string) error { return nil },
		nil,
		func(ctx context.Context, runnerID string) error {
			removeMu.Lock()
			removedCount++
			removeMu.Unlock()
			return nil
		},
	)

	ctx := context.Background()

	// Add some runners
	for i := 1; i <= 3; i++ {
		runner := &Runner{
			ID:              "test-runner-" + string(rune('0'+i)),
			State:           StateIdle,
			SnapshotVersion: "v1",
		}
		pooled := &pooledRunner{
			Runner: runner,
			key: &RunnerKey{
				SnapshotVersion: "v1",
				Platform:        "linux/amd64",
			},
		}
		if err := pool.Add(ctx, pooled); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	// Shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := pool.Shutdown(shutdownCtx)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// All runners should be removed
	removeMu.Lock()
	if removedCount != 3 {
		t.Errorf("Expected 3 runners to be removed, got %d", removedCount)
	}
	removeMu.Unlock()

	// Pool should be empty
	if pool.Len() != 0 {
		t.Errorf("Expected 0 pooled runners after shutdown, got %d", pool.Len())
	}
}

func TestPoolConcurrency(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxPooledRunners = 50

	pool := NewPool(cfg, logger)

	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error {
			time.Sleep(time.Millisecond) // Simulate pause latency
			return nil
		},
		func(ctx context.Context, runnerID string) error {
			time.Sleep(time.Millisecond) // Simulate resume latency
			return nil
		},
		nil,
		func(ctx context.Context, runnerID string) error { return nil },
	)

	ctx := context.Background()

	var wg sync.WaitGroup
	const numGoroutines = 10
	const opsPerGoroutine = 10

	// Concurrent adds
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				runner := &Runner{
					ID:              "runner-" + string(rune('a'+id)) + string(rune('0'+j)),
					State:           StateIdle,
					SnapshotVersion: "v1",
				}
				pooled := &pooledRunner{
					Runner: runner,
					key: &RunnerKey{
						SnapshotVersion: "v1",
						Platform:        "linux/amd64",
					},
				}
				_ = pool.Add(ctx, pooled)
			}
		}(i)
	}

	wg.Wait()

	// Pool should not exceed max capacity
	if pool.Len() > cfg.MaxPooledRunners {
		t.Errorf("Pool size %d exceeds max %d", pool.Len(), cfg.MaxPooledRunners)
	}
}

func TestKeysMatch(t *testing.T) {
	pool := NewPool(DefaultPoolConfig(), logrus.New())

	tests := []struct {
		name     string
		a, b     *RunnerKey
		expected bool
	}{
		{
			name:     "both nil",
			a:        nil,
			b:        nil,
			expected: true,
		},
		{
			name:     "one nil",
			a:        &RunnerKey{SnapshotVersion: "v1"},
			b:        nil,
			expected: false,
		},
		{
			name:     "matching versions",
			a:        &RunnerKey{SnapshotVersion: "v1"},
			b:        &RunnerKey{SnapshotVersion: "v1"},
			expected: true,
		},
		{
			name:     "different versions",
			a:        &RunnerKey{SnapshotVersion: "v1"},
			b:        &RunnerKey{SnapshotVersion: "v2"},
			expected: false,
		},
		{
			name:     "matching platforms",
			a:        &RunnerKey{SnapshotVersion: "v1", Platform: "linux/amd64"},
			b:        &RunnerKey{SnapshotVersion: "v1", Platform: "linux/amd64"},
			expected: true,
		},
		{
			name:     "different platforms",
			a:        &RunnerKey{SnapshotVersion: "v1", Platform: "linux/amd64"},
			b:        &RunnerKey{SnapshotVersion: "v1", Platform: "linux/arm64"},
			expected: false,
		},
		{
			name:     "empty platform matches any",
			a:        &RunnerKey{SnapshotVersion: "v1", Platform: ""},
			b:        &RunnerKey{SnapshotVersion: "v1", Platform: "linux/amd64"},
			expected: true,
		},
		{
			name:     "matching repos",
			a:        &RunnerKey{SnapshotVersion: "v1", AffinityKey: "org/repo"},
			b:        &RunnerKey{SnapshotVersion: "v1", AffinityKey: "org/repo"},
			expected: true,
		},
		{
			name:     "different repos",
			a:        &RunnerKey{SnapshotVersion: "v1", AffinityKey: "org/repo1"},
			b:        &RunnerKey{SnapshotVersion: "v1", AffinityKey: "org/repo2"},
			expected: false,
		},
		{
			name:     "empty repo matches any",
			a:        &RunnerKey{SnapshotVersion: "v1", AffinityKey: ""},
			b:        &RunnerKey{SnapshotVersion: "v1", AffinityKey: "org/repo"},
			expected: true,
		},
		{
			name: "matching labels",
			a: &RunnerKey{
				SnapshotVersion: "v1",
				Labels:          map[string]string{"env": "prod"},
			},
			b: &RunnerKey{
				SnapshotVersion: "v1",
				Labels:          map[string]string{"env": "prod", "team": "platform"},
			},
			expected: true, // pooled runner has extra labels, which is fine
		},
		{
			name: "missing required labels",
			a: &RunnerKey{
				SnapshotVersion: "v1",
				Labels:          map[string]string{"env": "prod", "special": "true"},
			},
			b: &RunnerKey{
				SnapshotVersion: "v1",
				Labels:          map[string]string{"env": "prod"},
			},
			expected: false, // request requires "special" label that pooled runner doesn't have
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pool.keysMatch(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("keysMatch(%v, %v) = %v, expected %v", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

func TestTryRecycle(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxRunnerMemoryBytes = 2 * 1024 * 1024 * 1024 // 2GB limit per runner

	pool := NewPool(cfg, logger)

	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error { return nil },
		func(ctx context.Context, runnerID string) error { return nil },
		func(ctx context.Context, runnerID string) (*VMStats, error) {
			return &VMStats{
				MemoryUsageBytes: 1024 * 1024 * 1024,     // 1GB
				DiskUsageBytes:   5 * 1024 * 1024 * 1024, // 5GB
			}, nil
		},
		nil,
	)

	ctx := context.Background()

	// Test successful recycle
	runner := &Runner{
		ID:              "test-runner-1",
		State:           StateIdle,
		SnapshotVersion: "v1",
	}
	pooled := &pooledRunner{
		Runner: runner,
		key: &RunnerKey{
			SnapshotVersion: "v1",
			Platform:        "linux/amd64",
		},
	}

	err := pool.TryRecycle(ctx, pooled, true)
	if err != nil {
		t.Fatalf("TryRecycle failed: %v", err)
	}

	if pool.Len() != 1 {
		t.Errorf("Expected 1 pooled runner, got %d", pool.Len())
	}

	// Test recycle with unclean finish
	runner2 := &Runner{
		ID:              "test-runner-2",
		State:           StateIdle,
		SnapshotVersion: "v1",
	}
	pooled2 := &pooledRunner{
		Runner: runner2,
		key: &RunnerKey{
			SnapshotVersion: "v1",
			Platform:        "linux/amd64",
		},
	}

	err = pool.TryRecycle(ctx, pooled2, false) // finishedCleanly = false
	if err == nil {
		t.Error("TryRecycle should fail when task didn't finish cleanly")
	}

	// Pool should still have only 1 runner
	if pool.Len() != 1 {
		t.Errorf("Expected 1 pooled runner, got %d", pool.Len())
	}
}

func TestFlushOlderThan_EvictsStaleVersions(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxPooledRunners = 10

	pool := NewPool(cfg, logger)

	removedRunners := make(map[string]bool)
	var removeMu sync.Mutex
	pool.SetCallbacks(
		func(ctx context.Context, runnerID string) error { return nil },
		func(ctx context.Context, runnerID string) error { return nil },
		nil,
		func(ctx context.Context, runnerID string) error {
			removeMu.Lock()
			removedRunners[runnerID] = true
			removeMu.Unlock()
			return nil
		},
	)

	// Manually add runners with different versions (bypass pause)
	pool.mu.Lock()
	pool.runners = append(pool.runners, &pooledRunner{
		Runner: &Runner{ID: "old-1", State: StatePaused, SnapshotVersion: "v1"},
		key:    &RunnerKey{SnapshotVersion: "v1"},
	})
	pool.runners = append(pool.runners, &pooledRunner{
		Runner: &Runner{ID: "old-2", State: StatePaused, SnapshotVersion: "v1"},
		key:    &RunnerKey{SnapshotVersion: "v1"},
	})
	pool.runners = append(pool.runners, &pooledRunner{
		Runner: &Runner{ID: "current", State: StatePaused, SnapshotVersion: "v2"},
		key:    &RunnerKey{SnapshotVersion: "v2"},
	})
	pool.mu.Unlock()

	ctx := context.Background()
	evicted := pool.FlushOlderThan(ctx, "v2")

	if evicted != 2 {
		t.Errorf("FlushOlderThan evicted %d runners, want 2", evicted)
	}

	if pool.Len() != 1 {
		t.Errorf("Pool has %d runners after flush, want 1", pool.Len())
	}

	// Wait for async removals
	time.Sleep(100 * time.Millisecond)

	removeMu.Lock()
	if !removedRunners["old-1"] || !removedRunners["old-2"] {
		t.Error("Expected old runners to be removed")
	}
	if removedRunners["current"] {
		t.Error("Current version runner should not be removed")
	}
	removeMu.Unlock()
}

func TestFlushOlderThan_EmptyVersion(t *testing.T) {
	logger := logrus.New()
	pool := NewPool(DefaultPoolConfig(), logger)

	evicted := pool.FlushOlderThan(context.Background(), "")
	if evicted != 0 {
		t.Errorf("FlushOlderThan with empty version should return 0, got %d", evicted)
	}
}

func TestFlushOlderThan_NoMatches(t *testing.T) {
	logger := logrus.New()
	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	pool := NewPool(cfg, logger)

	pool.mu.Lock()
	pool.runners = append(pool.runners, &pooledRunner{
		Runner: &Runner{ID: "r1", State: StatePaused, SnapshotVersion: "v2"},
		key:    &RunnerKey{SnapshotVersion: "v2"},
	})
	pool.mu.Unlock()

	evicted := pool.FlushOlderThan(context.Background(), "v2")
	if evicted != 0 {
		t.Errorf("FlushOlderThan should evict 0 when all match, got %d", evicted)
	}
	if pool.Len() != 1 {
		t.Errorf("Pool should still have 1 runner, got %d", pool.Len())
	}
}

func TestFlushByDesiredVersions(t *testing.T) {
	logger := logrus.New()
	cfg := DefaultPoolConfig()
	cfg.Enabled = true
	cfg.MaxPooledRunners = 10
	pool := NewPool(cfg, logger)

	pool.SetCallbacks(nil, nil, nil,
		func(ctx context.Context, runnerID string) error { return nil },
	)

	pool.mu.Lock()
	pool.runners = append(pool.runners,
		&pooledRunner{
			Runner: &Runner{ID: "r1", State: StatePaused, SnapshotVersion: "v1", PoolAffinityKey: "org/repo-a"},
			key:    &RunnerKey{SnapshotVersion: "v1"},
		},
		&pooledRunner{
			Runner: &Runner{ID: "r2", State: StatePaused, SnapshotVersion: "v2", PoolAffinityKey: "org/repo-a"},
			key:    &RunnerKey{SnapshotVersion: "v2"},
		},
		&pooledRunner{
			Runner: &Runner{ID: "r3", State: StatePaused, SnapshotVersion: "v1", PoolAffinityKey: "org/repo-b"},
			key:    &RunnerKey{SnapshotVersion: "v1"},
		},
	)
	pool.mu.Unlock()

	desired := map[string]string{
		"org/repo-a": "v2", // r1 is stale for repo-a
		"org/repo-b": "v1", // r3 is current for repo-b
	}

	evicted := pool.FlushByDesiredVersions(context.Background(), desired)
	if evicted != 1 {
		t.Errorf("FlushByDesiredVersions evicted %d, want 1 (only r1)", evicted)
	}
	if pool.Len() != 2 {
		t.Errorf("Pool has %d runners, want 2", pool.Len())
	}
}

func TestFlushByDesiredVersions_EmptyMap(t *testing.T) {
	logger := logrus.New()
	pool := NewPool(DefaultPoolConfig(), logger)

	evicted := pool.FlushByDesiredVersions(context.Background(), nil)
	if evicted != 0 {
		t.Errorf("FlushByDesiredVersions with nil map should return 0, got %d", evicted)
	}
}
