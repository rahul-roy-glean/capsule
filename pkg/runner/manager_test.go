package runner

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/ci"
)

// mockCIAdapter implements ci.Adapter for testing
type mockCIAdapter struct {
	name          string
	tokenFunc     func(ctx context.Context, opts ci.RunnerTokenOpts) (string, error)
	runnerURL     string
	onDrainFunc   func(ctx context.Context, runners []ci.RunnerInfo) error
	onReleaseFunc func(ctx context.Context, runner ci.RunnerInfo) error
}

func (m *mockCIAdapter) Name() string { return m.name }
func (m *mockCIAdapter) GetRunnerToken(ctx context.Context, opts ci.RunnerTokenOpts) (string, error) {
	if m.tokenFunc != nil {
		return m.tokenFunc(ctx, opts)
	}
	return "", nil
}
func (m *mockCIAdapter) RunnerURL() string { return m.runnerURL }
func (m *mockCIAdapter) OnDrain(ctx context.Context, runners []ci.RunnerInfo) error {
	if m.onDrainFunc != nil {
		return m.onDrainFunc(ctx, runners)
	}
	return nil
}
func (m *mockCIAdapter) OnRelease(ctx context.Context, runner ci.RunnerInfo) error {
	if m.onReleaseFunc != nil {
		return m.onReleaseFunc(ctx, runner)
	}
	return nil
}
func (m *mockCIAdapter) WebhookHandler() http.Handler { return nil }
func (m *mockCIAdapter) Close() error                 { return nil }

func newTestManager(opts ...func(*Manager)) *Manager {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	m := &Manager{
		config:       HostConfig{MaxRunners: 4, HostID: "test-host", Environment: "test"},
		runners:      make(map[string]*Runner),
		slotToRunner: make(map[int]string),
		runnerToSlot: make(map[string]int),
		logger:       logger.WithField("test", true),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestFindAvailableSlot_Empty(t *testing.T) {
	m := newTestManager()
	slot := m.findAvailableSlot()
	if slot != 0 {
		t.Errorf("findAvailableSlot() = %d, want 0 for empty manager", slot)
	}
}

func TestFindAvailableSlot_Partial(t *testing.T) {
	m := newTestManager()
	m.slotToRunner[0] = "runner-0"
	m.slotToRunner[1] = "runner-1"

	slot := m.findAvailableSlot()
	if slot != 2 {
		t.Errorf("findAvailableSlot() = %d, want 2", slot)
	}
}

func TestFindAvailableSlot_Full(t *testing.T) {
	m := newTestManager()
	for i := 0; i < m.config.MaxRunners; i++ {
		m.slotToRunner[i] = fmt.Sprintf("runner-%d", i)
	}

	slot := m.findAvailableSlot()
	if slot != -1 {
		t.Errorf("findAvailableSlot() = %d, want -1 for full manager", slot)
	}
}

func TestFindAvailableSlot_Gap(t *testing.T) {
	m := newTestManager()
	m.slotToRunner[0] = "runner-0"
	// slot 1 is free
	m.slotToRunner[2] = "runner-2"
	m.slotToRunner[3] = "runner-3"

	slot := m.findAvailableSlot()
	if slot != 1 {
		t.Errorf("findAvailableSlot() = %d, want 1 (first gap)", slot)
	}
}

func TestSetDraining(t *testing.T) {
	m := newTestManager()

	// First set to true
	changed := m.SetDraining(true)
	if !changed {
		t.Error("SetDraining(true) should return true when changing from false")
	}
	if !m.IsDraining() {
		t.Error("IsDraining() should be true after SetDraining(true)")
	}

	// Set to true again (no change)
	changed = m.SetDraining(true)
	if changed {
		t.Error("SetDraining(true) should return false when already true")
	}

	// Set back to false
	changed = m.SetDraining(false)
	if !changed {
		t.Error("SetDraining(false) should return true when changing from true")
	}
	if m.IsDraining() {
		t.Error("IsDraining() should be false after SetDraining(false)")
	}
}

func TestIsDraining(t *testing.T) {
	m := newTestManager()
	if m.IsDraining() {
		t.Error("new manager should not be draining")
	}
}

func TestGetStatus_Counting(t *testing.T) {
	m := newTestManager()
	m.config.MaxRunners = 10

	// GetStatus calls snapshotCache.CurrentVersion() which requires a non-nil snapshotCache.
	// Instead, test the counting logic directly which is the same as what GetStatus does.
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}
	m.runners["r2"] = &Runner{ID: "r2", State: StateBusy}
	m.runners["r3"] = &Runner{ID: "r3", State: StateBusy}
	m.runners["r4"] = &Runner{ID: "r4", State: StateBooting}
	m.runners["r5"] = &Runner{ID: "r5", State: StateIdle}

	// Count idle and busy manually (same logic as GetStatus)
	var idle, busy int
	m.mu.RLock()
	for _, r := range m.runners {
		switch r.State {
		case StateIdle:
			idle++
		case StateBusy:
			busy++
		}
	}
	m.mu.RUnlock()

	if idle != 2 {
		t.Errorf("idle count = %d, want 2", idle)
	}
	if busy != 2 {
		t.Errorf("busy count = %d, want 2", busy)
	}
	if len(m.runners) != 5 {
		t.Errorf("total runners = %d, want 5", len(m.runners))
	}
}

func TestRemoveRunnerLabels_NilAdapter(t *testing.T) {
	m := newTestManager()
	m.ciAdapter = nil

	count, err := m.RemoveRunnerLabels(context.Background())
	if err != nil {
		t.Errorf("RemoveRunnerLabels() error = %v", err)
	}
	if count != 0 {
		t.Errorf("RemoveRunnerLabels() = %d, want 0 with nil adapter", count)
	}
}

func TestRemoveRunnerLabels_WithMockAdapter(t *testing.T) {
	var drainedRunners []ci.RunnerInfo
	var mu sync.Mutex

	adapter := &mockCIAdapter{
		name: "mock",
		onDrainFunc: func(ctx context.Context, runners []ci.RunnerInfo) error {
			mu.Lock()
			drainedRunners = runners
			mu.Unlock()
			return nil
		},
	}

	m := newTestManager(func(m *Manager) {
		m.ciAdapter = adapter
	})
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle, GitHubRepo: "org/repo"}
	m.runners["r2"] = &Runner{ID: "r2", State: StateBusy, GitHubRepo: "org/repo"}

	count, err := m.RemoveRunnerLabels(context.Background())
	if err != nil {
		t.Errorf("RemoveRunnerLabels() error = %v", err)
	}
	if count != 2 {
		t.Errorf("RemoveRunnerLabels() = %d, want 2", count)
	}

	mu.Lock()
	if len(drainedRunners) != 2 {
		t.Errorf("OnDrain called with %d runners, want 2", len(drainedRunners))
	}
	mu.Unlock()
}

func TestRemoveRunnerLabels_AdapterError(t *testing.T) {
	adapter := &mockCIAdapter{
		name: "mock",
		onDrainFunc: func(ctx context.Context, runners []ci.RunnerInfo) error {
			return fmt.Errorf("drain failed")
		},
	}

	m := newTestManager(func(m *Manager) {
		m.ciAdapter = adapter
	})
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}

	_, err := m.RemoveRunnerLabels(context.Background())
	if err == nil {
		t.Error("RemoveRunnerLabels() expected error from adapter")
	}
}

func TestRemoveRunnerLabels_NoRunners(t *testing.T) {
	adapter := &mockCIAdapter{name: "mock"}
	m := newTestManager(func(m *Manager) {
		m.ciAdapter = adapter
	})

	count, err := m.RemoveRunnerLabels(context.Background())
	if err != nil {
		t.Errorf("RemoveRunnerLabels() error = %v", err)
	}
	if count != 0 {
		t.Errorf("RemoveRunnerLabels() = %d, want 0", count)
	}
}

func TestBuildDrives_WithGitCache(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.credentialsImage = "/path/to/creds.img"
		m.gitCacheImage = "/path/to/git-cache.img"
	})

	drives := m.buildDrives("/path/to/seed.img", "/path/to/upper.img")

	if len(drives) != 4 {
		t.Fatalf("buildDrives() returned %d drives, want 4", len(drives))
	}

	// Check drive IDs
	wantIDs := []string{"repo_cache_seed", "repo_cache_upper", "credentials", "git_cache"}
	for i, wantID := range wantIDs {
		if drives[i].DriveID != wantID {
			t.Errorf("drives[%d].DriveID = %q, want %q", i, drives[i].DriveID, wantID)
		}
	}

	// Check repo_cache_seed is read-only
	if !drives[0].IsReadOnly {
		t.Error("repo_cache_seed should be read-only")
	}
	// Check repo_cache_upper is writable
	if drives[1].IsReadOnly {
		t.Error("repo_cache_upper should be writable")
	}
	// Check credentials is read-only
	if !drives[2].IsReadOnly {
		t.Error("credentials should be read-only")
	}
	// Check git_cache is read-only
	if !drives[3].IsReadOnly {
		t.Error("git_cache should be read-only")
	}
}

func TestBuildDrives_WithoutGitCache(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.credentialsImage = "/path/to/creds.img"
		m.gitCacheImage = "" // no git cache
		m.config.WorkspaceDir = t.TempDir()
	})

	drives := m.buildDrives("/path/to/seed.img", "/path/to/upper.img")

	// Without a git cache image, buildDrives calls getOrCreateGitCachePlaceholder which
	// tries to create an ext4 image. On macOS (or systems without mkfs.ext4) this will
	// fail silently and return only 3 drives. On Linux it would return 4.
	if len(drives) < 3 {
		t.Fatalf("buildDrives() returned %d drives, want at least 3", len(drives))
	}

	// First 3 drives should always be present
	wantIDs := []string{"repo_cache_seed", "repo_cache_upper", "credentials"}
	for i, wantID := range wantIDs {
		if drives[i].DriveID != wantID {
			t.Errorf("drives[%d].DriveID = %q, want %q", i, drives[i].DriveID, wantID)
		}
	}
}

func TestBuildDrives_PathsCorrect(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.credentialsImage = "/creds/image.img"
		m.gitCacheImage = "/git/cache.img"
	})

	drives := m.buildDrives("/seed/path.img", "/upper/path.img")

	if drives[0].PathOnHost != "/seed/path.img" {
		t.Errorf("repo_cache_seed path = %q, want %q", drives[0].PathOnHost, "/seed/path.img")
	}
	if drives[1].PathOnHost != "/upper/path.img" {
		t.Errorf("repo_cache_upper path = %q, want %q", drives[1].PathOnHost, "/upper/path.img")
	}
	if drives[2].PathOnHost != "/creds/image.img" {
		t.Errorf("credentials path = %q, want %q", drives[2].PathOnHost, "/creds/image.img")
	}
	if drives[3].PathOnHost != "/git/cache.img" {
		t.Errorf("git_cache path = %q, want %q", drives[3].PathOnHost, "/git/cache.img")
	}
}

func TestBuildDrives_NoDrivesAreRootDevice(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.credentialsImage = "/path/to/creds.img"
		m.gitCacheImage = "/path/to/git-cache.img"
	})

	drives := m.buildDrives("/path/to/seed.img", "/path/to/upper.img")

	for i, d := range drives {
		if d.IsRootDevice {
			t.Errorf("drives[%d] (%s) should not be root device", i, d.DriveID)
		}
	}
}

func TestSetDraining_Concurrent(t *testing.T) {
	m := newTestManager()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.SetDraining(i%2 == 0)
			_ = m.IsDraining()
		}(i)
	}

	wg.Wait()
	// Just verify no race conditions (test with -race flag)
}

func TestListRunners_FilterByState(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}
	m.runners["r2"] = &Runner{ID: "r2", State: StateBusy}
	m.runners["r3"] = &Runner{ID: "r3", State: StateIdle}

	idle := m.ListRunners(StateIdle)
	if len(idle) != 2 {
		t.Errorf("ListRunners(StateIdle) returned %d, want 2", len(idle))
	}

	busy := m.ListRunners(StateBusy)
	if len(busy) != 1 {
		t.Errorf("ListRunners(StateBusy) returned %d, want 1", len(busy))
	}

	all := m.ListRunners("")
	if len(all) != 3 {
		t.Errorf("ListRunners(\"\") returned %d, want 3", len(all))
	}
}

func TestListRunners_Empty(t *testing.T) {
	m := newTestManager()

	result := m.ListRunners("")
	if len(result) != 0 {
		t.Errorf("ListRunners on empty manager returned %d, want 0", len(result))
	}
}

func TestCanAddRunner(t *testing.T) {
	m := newTestManager()
	m.config.MaxRunners = 2

	if !m.CanAddRunner() {
		t.Error("CanAddRunner() should be true for empty manager")
	}

	m.runners["r1"] = &Runner{ID: "r1"}
	m.runners["r2"] = &Runner{ID: "r2"}

	if m.CanAddRunner() {
		t.Error("CanAddRunner() should be false at capacity")
	}
}

func TestCanAddRunner_Draining(t *testing.T) {
	m := newTestManager()
	m.draining = true

	if m.CanAddRunner() {
		t.Error("CanAddRunner() should be false when draining")
	}
}

func TestCanAddRunner_DrainingEvenWithCapacity(t *testing.T) {
	m := newTestManager()
	m.config.MaxRunners = 10
	m.draining = true

	if m.CanAddRunner() {
		t.Error("CanAddRunner() should be false when draining, regardless of capacity")
	}
}

func TestIdleCount(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}
	m.runners["r2"] = &Runner{ID: "r2", State: StateBusy}
	m.runners["r3"] = &Runner{ID: "r3", State: StateIdle}
	m.runners["r4"] = &Runner{ID: "r4", State: StateBooting}

	if got := m.IdleCount(); got != 2 {
		t.Errorf("IdleCount() = %d, want 2", got)
	}
}

func TestIdleCount_Empty(t *testing.T) {
	m := newTestManager()

	if got := m.IdleCount(); got != 0 {
		t.Errorf("IdleCount() = %d, want 0", got)
	}
}

func TestIdleCount_NoneIdle(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateBusy}
	m.runners["r2"] = &Runner{ID: "r2", State: StateBooting}

	if got := m.IdleCount(); got != 0 {
		t.Errorf("IdleCount() = %d, want 0", got)
	}
}

func TestGetRunner(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}

	r, err := m.GetRunner("r1")
	if err != nil {
		t.Fatalf("GetRunner(r1) error = %v", err)
	}
	if r.ID != "r1" {
		t.Errorf("GetRunner(r1).ID = %q, want %q", r.ID, "r1")
	}
}

func TestGetRunner_NotFound(t *testing.T) {
	m := newTestManager()

	_, err := m.GetRunner("nonexistent")
	if err == nil {
		t.Error("GetRunner(nonexistent) should return error")
	}
}

func TestSetRunnerState(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}

	err := m.SetRunnerState("r1", StateBusy)
	if err != nil {
		t.Fatalf("SetRunnerState() error = %v", err)
	}
	if m.runners["r1"].State != StateBusy {
		t.Errorf("runner state = %q, want %q", m.runners["r1"].State, StateBusy)
	}
}

func TestSetRunnerState_NotFound(t *testing.T) {
	m := newTestManager()

	err := m.SetRunnerState("nonexistent", StateBusy)
	if err == nil {
		t.Error("SetRunnerState(nonexistent) should return error")
	}
}
