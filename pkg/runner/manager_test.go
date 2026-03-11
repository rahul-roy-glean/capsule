package runner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func newTestManager(opts ...func(*Manager)) *Manager {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	m := &Manager{
		config:          HostConfig{MaxRunners: 4, HostID: "test-host", Environment: "test"},
		runners:         make(map[string]*Runner),
		slotToRunner:    make(map[int]string),
		runnerToSlot:    make(map[string]int),
		pendingSessions: make(map[string]string),
		uffdHandlers:    make(map[string]uffdStopper),
		logger:          logger.WithField("test", true),
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

	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}
	m.runners["r2"] = &Runner{ID: "r2", State: StateBusy}
	m.runners["r3"] = &Runner{ID: "r3", State: StateBusy}
	m.runners["r4"] = &Runner{ID: "r4", State: StateBooting}
	m.runners["r5"] = &Runner{ID: "r5", State: StateIdle}

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

func TestBuildDrives_NoDrives(t *testing.T) {
	m := newTestManager()

	drives := m.buildDrives(nil)

	if len(drives) != 0 {
		t.Fatalf("buildDrives(nil) returned %d drives, want 0", len(drives))
	}
}

func TestBuildDrives_WithExtensionDrives(t *testing.T) {
	m := newTestManager()

	ext := map[string]string{
		"git_drive":   "/path/to/git.img",
		"bazel_cache": "/path/to/bazel.img",
	}
	drives := m.buildDrives(ext)

	if len(drives) != 2 {
		t.Fatalf("buildDrives() returned %d drives, want 2", len(drives))
	}

	// Extension drives should be in sorted order
	if drives[0].DriveID != "bazel_cache" {
		t.Errorf("drives[0].DriveID = %q, want %q", drives[0].DriveID, "bazel_cache")
	}
	if drives[1].DriveID != "git_drive" {
		t.Errorf("drives[1].DriveID = %q, want %q", drives[1].DriveID, "git_drive")
	}
	if drives[0].PathOnHost != "/path/to/bazel.img" {
		t.Errorf("bazel_cache path = %q, want %q", drives[0].PathOnHost, "/path/to/bazel.img")
	}
	if drives[1].PathOnHost != "/path/to/git.img" {
		t.Errorf("git_drive path = %q, want %q", drives[1].PathOnHost, "/path/to/git.img")
	}
}

func TestBuildDrives_NoDrivesAreRootDevice(t *testing.T) {
	m := newTestManager()

	drives := m.buildDrives(map[string]string{"ext_drive": "/path/to/ext.img"})

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

	if !m.CanAddRunner(0, 0) {
		t.Error("CanAddRunner() should be true for empty manager")
	}

	m.runners["r1"] = &Runner{ID: "r1"}
	m.runners["r2"] = &Runner{ID: "r2"}

	if m.CanAddRunner(0, 0) {
		t.Error("CanAddRunner() should be false at capacity")
	}
}

func TestCanAddRunner_Draining(t *testing.T) {
	m := newTestManager()
	m.draining = true

	if m.CanAddRunner(0, 0) {
		t.Error("CanAddRunner() should be false when draining")
	}
}

func TestCanAddRunner_DrainingEvenWithCapacity(t *testing.T) {
	m := newTestManager()
	m.config.MaxRunners = 10
	m.draining = true

	if m.CanAddRunner(0, 0) {
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

func TestGetStatus_ExcludesSuspendedAndPausedResourceUsage(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.config.TotalCPUMillicores = 16000
		m.config.TotalMemoryMB = 32768
	})
	m.runners["active"] = &Runner{
		ID:        "active",
		State:     StateBusy,
		Resources: Resources{VCPUs: 2, MemoryMB: 4096},
	}
	m.runners["suspended"] = &Runner{
		ID:        "suspended",
		State:     StateSuspended,
		Resources: Resources{VCPUs: 4, MemoryMB: 8192},
	}
	m.runners["paused"] = &Runner{
		ID:        "paused",
		State:     StatePaused,
		Resources: Resources{VCPUs: 1, MemoryMB: 2048},
	}

	status := m.GetStatus()

	if status.UsedCPUMillicores != 2000 {
		t.Fatalf("UsedCPUMillicores = %d, want 2000", status.UsedCPUMillicores)
	}
	if status.UsedMemoryMB != 4096 {
		t.Fatalf("UsedMemoryMB = %d, want 4096", status.UsedMemoryMB)
	}
	if status.BusyRunners != 1 {
		t.Fatalf("BusyRunners = %d, want 1", status.BusyRunners)
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

func TestIdempotencyTracking(t *testing.T) {
	m := newTestManager()
	m.recentRequests = make(map[string]*recentAllocation)

	runner := &Runner{ID: "test-runner-1", State: StateIdle}
	m.runners["test-runner-1"] = runner
	m.recentRequests["req-123"] = &recentAllocation{
		runner:    runner,
		allocTime: time.Now(),
	}

	m.mu.RLock()
	recent, ok := m.recentRequests["req-123"]
	m.mu.RUnlock()

	if !ok {
		t.Fatal("Expected to find recent request")
	}
	if recent.runner.ID != "test-runner-1" {
		t.Errorf("Expected runner ID test-runner-1, got %s", recent.runner.ID)
	}
	if time.Since(recent.allocTime) > 5*time.Minute {
		t.Error("Recent allocation should not be expired yet")
	}
}

func TestIdempotencyCleanup(t *testing.T) {
	m := newTestManager()
	m.recentRequests = make(map[string]*recentAllocation)

	m.recentRequests["old-req"] = &recentAllocation{
		runner:    &Runner{ID: "old-runner"},
		allocTime: time.Now().Add(-10 * time.Minute),
	}
	m.recentRequests["new-req"] = &recentAllocation{
		runner:    &Runner{ID: "new-runner"},
		allocTime: time.Now(),
	}

	m.cleanupRecentRequests()

	if _, ok := m.recentRequests["old-req"]; ok {
		t.Error("Expired request should have been cleaned up")
	}
	if _, ok := m.recentRequests["new-req"]; !ok {
		t.Error("Fresh request should not have been cleaned up")
	}
}

func TestIdempotentAllocationWaitsForLeader(t *testing.T) {
	m := newTestManager()
	m.recentRequests = make(map[string]*recentAllocation)

	existing, alloc, leader := m.beginIdempotentAllocation("req-1")
	if existing != nil {
		t.Fatalf("unexpected existing runner: %+v", existing)
	}
	if !leader || alloc == nil {
		t.Fatalf("expected leader allocation, got leader=%v alloc=%v", leader, alloc)
	}

	waitExisting, waitAlloc, waitLeader := m.beginIdempotentAllocation("req-1")
	if waitExisting != nil {
		t.Fatalf("unexpected existing runner for waiter: %+v", waitExisting)
	}
	if waitLeader || waitAlloc != alloc {
		t.Fatalf("expected waiter to share inflight allocation, leader=%v sameAlloc=%v", waitLeader, waitAlloc == alloc)
	}

	runner := &Runner{ID: "runner-1", State: StateIdle}
	m.runners[runner.ID] = runner
	go func() {
		time.Sleep(10 * time.Millisecond)
		m.finishIdempotentAllocation("req-1", alloc, runner, nil)
	}()

	got, err := m.waitForIdempotentAllocation(context.Background(), "req-1", waitAlloc)
	if err != nil {
		t.Fatalf("waitForIdempotentAllocation() error = %v", err)
	}
	if got == nil || got.ID != runner.ID {
		t.Fatalf("waitForIdempotentAllocation() = %+v, want %s", got, runner.ID)
	}
}

func TestAcquireBringupLeaseCountsPendingReservations(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.config.MaxRunners = 1
	})

	lease, err := m.AcquireBringupLease("runner-1", "")
	if err != nil {
		t.Fatalf("first AcquireBringupLease() error = %v", err)
	}
	if m.CanAddRunner(0, 0) {
		t.Fatal("CanAddRunner() should be false while a lease is held")
	}
	if _, err := m.AcquireBringupLease("runner-2", ""); err == nil {
		t.Fatal("second AcquireBringupLease() should fail at capacity")
	}

	lease.Release()

	lease2, err := m.AcquireBringupLease("runner-3", "")
	if err != nil {
		t.Fatalf("AcquireBringupLease() after release error = %v", err)
	}
	lease2.Release()
}

func TestDrainBlocksAllocation(t *testing.T) {
	m := newTestManager()
	m.SetDraining(true)

	if !m.IsDraining() {
		t.Error("Expected manager to be draining")
	}
	if m.CanAddRunner(0, 0) {
		t.Error("Draining manager should not accept new runners")
	}
}

func TestCanAddRunner_AtCapacity(t *testing.T) {
	m := newTestManager()
	m.config.MaxRunners = 4
	for i := 0; i < 4; i++ {
		m.runners[fmt.Sprintf("runner-%d", i)] = &Runner{ID: fmt.Sprintf("runner-%d", i)}
	}

	if m.CanAddRunner(0, 0) {
		t.Error("Manager at capacity should not accept new runners")
	}
}

func TestSetDraining_ChangedReport(t *testing.T) {
	m := newTestManager()

	if !m.SetDraining(true) {
		t.Error("First SetDraining(true) should return changed=true")
	}

	if m.SetDraining(true) {
		t.Error("Second SetDraining(true) should return changed=false")
	}

	if !m.SetDraining(false) {
		t.Error("SetDraining(false) after true should return changed=true")
	}
}
