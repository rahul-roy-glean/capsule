//go:build linux
// +build linux

package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/authproxy"
	"github.com/rahul-roy-glean/capsule/pkg/firecracker"
	"github.com/rahul-roy-glean/capsule/pkg/fuse"
	"github.com/rahul-roy-glean/capsule/pkg/network"
	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
	"github.com/rahul-roy-glean/capsule/pkg/uffd"
)

func newTestChunkedManager() *ChunkedManager {
	logger := logrus.New()
	base := newTestManager()
	return &ChunkedManager{
		Manager:            base,
		uffdHandlers:       make(map[string]*uffd.Handler),
		fuseDisks:          make(map[string]*fuse.ChunkedDisk),
		fuseExtensionDisks: make(map[string]map[string]*fuse.ChunkedDisk),
		chunkedLogger:      logger.WithField("test", true),
	}
}

type fakeChunkedVM struct {
	actions    *[]string
	restoreErr error
	startErr   error
	resumeErr  error
	mmdsErr    error
}

func (f *fakeChunkedVM) RestoreFromSnapshot(ctx context.Context, snapshotPath, memPath string, resume bool) error {
	*f.actions = append(*f.actions, fmt.Sprintf("restore-file:%s:%s", snapshotPath, memPath))
	return f.restoreErr
}

func (f *fakeChunkedVM) RestoreFromSnapshotWithUFFD(ctx context.Context, snapshotPath, uffdSocketPath string, resume bool) error {
	*f.actions = append(*f.actions, fmt.Sprintf("restore-uffd:%s:%s", snapshotPath, uffdSocketPath))
	return f.restoreErr
}

func (f *fakeChunkedVM) Start(ctx context.Context) error {
	*f.actions = append(*f.actions, "start")
	return f.startErr
}

func (f *fakeChunkedVM) Resume(ctx context.Context) error {
	*f.actions = append(*f.actions, "resume")
	return f.resumeErr
}

func (f *fakeChunkedVM) Stop() error {
	*f.actions = append(*f.actions, "stop")
	return nil
}

func (f *fakeChunkedVM) SetMMDSData(ctx context.Context, data interface{}) error {
	*f.actions = append(*f.actions, "set-mmds")
	return f.mmdsErr
}

func TestAcquireBringupLeaseConcurrentUniqueSlots(t *testing.T) {
	cm := newTestChunkedManager()

	type result struct {
		slot int
		err  error
	}

	start := make(chan struct{})
	results := make(chan result, 3)
	var wg sync.WaitGroup

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			runnerID := "runner-" + string(rune('a'+id))
			lease, err := cm.AcquireBringupLease(runnerID, "")
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{slot: lease.slot}
		}(i)
	}

	close(start)
	wg.Wait()
	close(results)

	var slots []int
	failures := 0
	for res := range results {
		if res.err != nil {
			failures++
			continue
		}
		slots = append(slots, res.slot)
	}

	if failures != 0 {
		t.Fatalf("expected 0 reservation failures, got %d", failures)
	}
	sort.Ints(slots)
	if len(slots) != 3 || slots[0] != 0 || slots[1] != 1 || slots[2] != 2 {
		t.Fatalf("expected successful reservations for slots [0 1 2], got %v", slots)
	}
}

func TestBringupLeaseReleaseAllowsReuse(t *testing.T) {
	cm := newTestChunkedManager()

	lease1, err := cm.AcquireBringupLease("runner-1", "")
	if err != nil {
		t.Fatalf("AcquireBringupLease(runner-1) error = %v", err)
	}
	if lease1.slot != 0 {
		t.Fatalf("lease1.slot = %d, want 0", lease1.slot)
	}

	lease2, err := cm.AcquireBringupLease("runner-2", "")
	if err != nil {
		t.Fatalf("AcquireBringupLease(runner-2) error = %v", err)
	}
	if lease2.slot != 1 {
		t.Fatalf("lease2.slot = %d, want 1", lease2.slot)
	}

	lease1.Release()

	lease3, err := cm.AcquireBringupLease("runner-3", "")
	if err != nil {
		t.Fatalf("AcquireBringupLease(runner-3) after release error = %v", err)
	}
	if lease3.slot != 0 {
		t.Fatalf("lease3.slot = %d, want reused slot 0", lease3.slot)
	}
	lease2.Release()
	lease3.Release()
}

func TestRegisterAllocatedRunnerPublishesResources(t *testing.T) {
	cm := newTestChunkedManager()

	runner := &Runner{ID: "runner-1", State: StateIdle}
	vm := &firecracker.VM{}
	fuseDisk := &fuse.ChunkedDisk{}
	extensionDisks := map[string]*fuse.ChunkedDisk{
		"ext": {},
	}
	uffdHandler := &uffd.Handler{}
	proxy := &authproxy.AuthProxy{}

	if err := cm.registerAllocatedRunner("runner-1", runner, vm, fuseDisk, extensionDisks, uffdHandler, proxy); err != nil {
		t.Fatalf("registerAllocatedRunner() error = %v", err)
	}

	if got := cm.runners["runner-1"]; got != runner {
		t.Fatalf("runner not published correctly: got %+v want %+v", got, runner)
	}
	if got := cm.vms["runner-1"]; got != vm {
		t.Fatalf("vm not published correctly: got %+v want %+v", got, vm)
	}
	if got := cm.fuseDisks["runner-1"]; got != fuseDisk {
		t.Fatalf("fuse disk not published correctly: got %+v want %+v", got, fuseDisk)
	}
	if got := cm.fuseExtensionDisks["runner-1"]["ext"]; got != extensionDisks["ext"] {
		t.Fatalf("extension disk not published correctly: got %+v want %+v", got, extensionDisks["ext"])
	}
	if got := cm.uffdHandlers["runner-1"]; got != uffdHandler {
		t.Fatalf("uffd handler not published correctly: got %+v want %+v", got, uffdHandler)
	}
	if got := cm.authProxies["runner-1"]; got != proxy {
		t.Fatalf("auth proxy not published correctly: got %+v want %+v", got, proxy)
	}
}

func TestRegisterAllocatedRunnerRejectsDrainingHost(t *testing.T) {
	cm := newTestChunkedManager()
	cm.draining = true

	err := cm.registerAllocatedRunner("runner-1", &Runner{ID: "runner-1"}, &firecracker.VM{}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("registerAllocatedRunner() should fail while draining")
	}
	if len(cm.runners) != 0 || len(cm.vms) != 0 {
		t.Fatalf("draining manager should not publish runner resources, got runners=%d vms=%d", len(cm.runners), len(cm.vms))
	}
}

func TestCleanupChunkedRunnerRemovesArtifacts(t *testing.T) {
	cm := newTestChunkedManager()
	cm.config.WorkspaceDir = t.TempDir()
	cm.config.SocketDir = t.TempDir()

	workspaceDir := filepath.Join(cm.config.WorkspaceDir, "runner-1")
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		t.Fatalf("MkdirAll(workspaceDir) error = %v", err)
	}
	sockPath := filepath.Join(cm.config.SocketDir, "runner-1.sock")
	uffdSockPath := filepath.Join(cm.config.SocketDir, "runner-1.uffd.sock")
	for _, path := range []string{sockPath, uffdSockPath} {
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}

	cm.cleanupChunkedRunner("runner-1", nil, nil, nil, nil)

	if _, err := os.Stat(workspaceDir); !os.IsNotExist(err) {
		t.Fatalf("workspaceDir should be removed, stat err = %v", err)
	}
	for _, path := range []string{sockPath, uffdSockPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, stat err = %v", path, err)
		}
	}
}

func TestBringupLeaseReleaseCleansUpSlot(t *testing.T) {
	cm := newTestChunkedManager()

	lease, err := cm.AcquireBringupLease("runner-1", "")
	if err != nil {
		t.Fatalf("AcquireBringupLease() error = %v", err)
	}

	if _, ok := cm.runnerToSlot["runner-1"]; !ok {
		t.Fatal("AcquireBringupLease() should register runnerToSlot entry")
	}
	if _, ok := cm.slotToRunner[lease.slot]; !ok {
		t.Fatal("AcquireBringupLease() should register slotToRunner entry")
	}

	lease.Release()

	if _, ok := cm.runnerToSlot["runner-1"]; ok {
		t.Fatal("Release() should clear runnerToSlot entry")
	}
	if _, ok := cm.slotToRunner[lease.slot]; ok {
		t.Fatal("Release() should clear slotToRunner entry")
	}
}

func TestEnsureKernelPathUsesExistingLocalCache(t *testing.T) {
	cm := newTestChunkedManager()
	cm.config.SnapshotCachePath = t.TempDir()

	kernelPath := filepath.Join(cm.config.SnapshotCachePath, "kernel.bin")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0644); err != nil {
		t.Fatalf("WriteFile(kernel.bin) error = %v", err)
	}

	got, err := cm.ensureKernelPath(context.Background(), nil)
	if err != nil {
		t.Fatalf("ensureKernelPath() error = %v", err)
	}
	if got != kernelPath {
		t.Fatalf("ensureKernelPath() = %q, want %q", got, kernelPath)
	}
}

func TestEnsureLocalMemFileUsesExistingLocalCache(t *testing.T) {
	cm := newTestChunkedManager()
	cm.config.SnapshotCachePath = t.TempDir()

	memPath := filepath.Join(cm.config.SnapshotCachePath, "wk", "snapshot.mem")
	if err := os.MkdirAll(filepath.Dir(memPath), 0755); err != nil {
		t.Fatalf("MkdirAll(mem dir) error = %v", err)
	}
	if err := os.WriteFile(memPath, []byte("mem"), 0644); err != nil {
		t.Fatalf("WriteFile(snapshot.mem) error = %v", err)
	}

	got, err := cm.ensureLocalMemFile(context.Background(), "runner-1", "wk", nil)
	if err != nil {
		t.Fatalf("ensureLocalMemFile() error = %v", err)
	}
	if got != memPath {
		t.Fatalf("ensureLocalMemFile() = %q, want %q", got, memPath)
	}
}

func TestRestoreAndActivateRunnerSequencesRestoreResumeAndReady(t *testing.T) {
	cm := newTestChunkedManager()

	actions := []string{}
	vm := &fakeChunkedVM{actions: &actions}
	cm.newVMFn = func(cfg firecracker.VMConfig, logger *logrus.Logger) (chunkedVM, error) {
		return vm, nil
	}
	cm.setupChunkedSymlinksFn = func(runnerID, rootfsPath string, extensionDrivePaths map[string]string) (string, func(), error) {
		actions = append(actions, "setup-symlinks")
		return "/tmp/snapshot-test", func() { actions = append(actions, "cleanup-symlinks") }, nil
	}
	cm.forwardPortFn = func(runnerID string, port int) error {
		actions = append(actions, fmt.Sprintf("forward:%d", port))
		return nil
	}
	cm.waitForReadyFn = func(ctx context.Context, ip string, timeout time.Duration) error {
		actions = append(actions, "ready")
		return nil
	}

	_, subnet, _ := net.ParseCIDR("10.0.0.0/24")
	tap := &network.TapDevice{
		Name:    "tap0",
		IP:      net.IPv4(10, 0, 0, 2),
		Gateway: net.IPv4(10, 0, 0, 1),
		Subnet:  subnet,
		MAC:     "02:fc:0a:00:00:02",
	}
	netns := &network.VMNamespace{
		Slot:    7,
		Path:    "/tmp/netns",
		Gateway: net.IPv4(10, 0, 0, 1),
	}
	runner := &Runner{
		ID:         "runner-1",
		HostID:     "host-1",
		InternalIP: net.IPv4(10, 0, 0, 2),
		Resources:  Resources{VCPUs: 2, MemoryMB: 2048},
	}

	vmCfg := firecracker.VMConfig{VMID: "runner-1", RootfsPath: "/tmp/rootfs.img"}
	gotVM, proxy, err := cm.restoreAndActivateRunner(
		context.Background(),
		"runner-1",
		AllocateRequest{},
		runner,
		netns,
		tap,
		vmCfg,
		"/tmp/state",
		"",
		"/tmp/uffd.sock",
		false,
		map[string]string{},
		nil,
	)
	if err != nil {
		t.Fatalf("restoreAndActivateRunner() error = %v", err)
	}
	if gotVM != vm {
		t.Fatalf("restoreAndActivateRunner() returned unexpected VM: %+v", gotVM)
	}
	if proxy != nil {
		t.Fatalf("expected no proxy for nil AuthConfig, got %+v", proxy)
	}

	want := []string{
		"setup-symlinks",
		"restore-uffd:/tmp/state:/tmp/uffd.sock",
		"set-mmds",
		"resume",
		fmt.Sprintf("forward:%d", snapshot.ThawAgentHealthPort),
		fmt.Sprintf("forward:%d", snapshot.ThawAgentDebugPort),
		"ready",
		"cleanup-symlinks", // deferred — runs last
	}
	if len(actions) != len(want) {
		t.Fatalf("unexpected action count: got %v want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("action[%d] = %q, want %q; full actions=%v", i, actions[i], want[i], actions)
		}
	}
}

func TestRestoreAndActivateRunnerFallsBackToColdBoot(t *testing.T) {
	cm := newTestChunkedManager()

	actions := []string{}
	firstVM := &fakeChunkedVM{actions: &actions, restoreErr: fmt.Errorf("restore failed")}
	secondVM := &fakeChunkedVM{actions: &actions}
	vmCalls := 0
	cm.newVMFn = func(cfg firecracker.VMConfig, logger *logrus.Logger) (chunkedVM, error) {
		vmCalls++
		if vmCalls == 1 {
			return firstVM, nil
		}
		return secondVM, nil
	}
	cm.setupChunkedSymlinksFn = func(runnerID, rootfsPath string, extensionDrivePaths map[string]string) (string, func(), error) {
		return "/tmp/snapshot-test", func() {}, nil
	}
	cm.forwardPortFn = func(runnerID string, port int) error { return nil }
	cm.waitForReadyFn = func(ctx context.Context, ip string, timeout time.Duration) error {
		actions = append(actions, "ready")
		return nil
	}

	_, subnet, _ := net.ParseCIDR("10.0.0.0/24")
	tap := &network.TapDevice{
		Name:    "tap0",
		IP:      net.IPv4(10, 0, 0, 2),
		Gateway: net.IPv4(10, 0, 0, 1),
		Subnet:  subnet,
		MAC:     "02:fc:0a:00:00:02",
	}
	netns := &network.VMNamespace{
		Slot:    7,
		Path:    "/tmp/netns",
		Gateway: net.IPv4(10, 0, 0, 1),
	}
	runner := &Runner{
		ID:         "runner-1",
		HostID:     "host-1",
		InternalIP: net.IPv4(10, 0, 0, 2),
		Resources:  Resources{VCPUs: 2, MemoryMB: 2048},
	}

	_, _, err := cm.restoreAndActivateRunner(
		context.Background(),
		"runner-1",
		AllocateRequest{},
		runner,
		netns,
		tap,
		firecracker.VMConfig{VMID: "runner-1", RootfsPath: "/tmp/rootfs.img"},
		"/tmp/state",
		"",
		"/tmp/uffd.sock",
		false,
		map[string]string{},
		nil,
	)
	if err != nil {
		t.Fatalf("restoreAndActivateRunner() fallback error = %v", err)
	}

	wantPrefix := []string{
		"restore-uffd:/tmp/state:/tmp/uffd.sock",
		"stop",
		"start",
		"set-mmds",
		"ready",
	}
	for i, want := range wantPrefix {
		if actions[i] != want {
			t.Fatalf("action[%d] = %q, want %q; full actions=%v", i, actions[i], want, actions)
		}
	}
	for _, action := range actions {
		if action == "resume" {
			t.Fatalf("fallback cold boot path should not call resume; actions=%v", actions)
		}
	}
}
