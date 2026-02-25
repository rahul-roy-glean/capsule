package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// VMConfig holds the complete configuration for a microVM
type VMConfig struct {
	VMID           string
	SocketDir      string
	FirecrackerBin string
	KernelPath     string
	RootfsPath     string
	VCPUs          int
	MemoryMB       int
	BootArgs       string
	NetworkIface   *NetworkInterface
	Vsock          *Vsock
	MMDSConfig     *MMDSConfig
	Drives         []Drive
	LogPath        string
	MetricsPath    string
	// NetNSPath is the path to a network namespace file (e.g., /var/run/netns/fc-xxxx).
	// When set, Firecracker is launched inside this namespace via "ip netns exec".
	NetNSPath string
}

// VM represents a running Firecracker microVM
type VM struct {
	client *Client
	config VMConfig
	logger *logrus.Entry
}

// consolePath returns the path for guest serial console logs,
// derived from LogPath by replacing the extension with .console.log.
// Returns empty string if LogPath is not set.
func (vm *VM) consolePath() string {
	if vm.config.LogPath == "" {
		return ""
	}
	ext := filepath.Ext(vm.config.LogPath)
	return strings.TrimSuffix(vm.config.LogPath, ext) + ".console.log"
}

// NewVM creates a new VM instance
func NewVM(cfg VMConfig, logger *logrus.Logger) (*VM, error) {
	if cfg.VMID == "" {
		cfg.VMID = uuid.New().String()
	}

	if cfg.SocketDir == "" {
		cfg.SocketDir = "/var/run/firecracker"
	}

	if cfg.VCPUs == 0 {
		cfg.VCPUs = 2
	}

	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = 1024
	}

	if cfg.BootArgs == "" {
		cfg.BootArgs = "console=ttyS0 reboot=k panic=1 pci=off"
	}

	socketPath := filepath.Join(cfg.SocketDir, cfg.VMID+".sock")

	client := NewClient(Config{
		SocketPath: socketPath,
		VMID:       cfg.VMID,
		NetNSPath:  cfg.NetNSPath,
		Logger:     logger,
	})

	return &VM{
		client: client,
		config: cfg,
		logger: logger.WithField("vm_id", cfg.VMID),
	}, nil
}

// Start boots the microVM from scratch (cold boot)
func (vm *VM) Start(ctx context.Context) error {
	vm.logger.Info("Starting microVM (cold boot)")

	// Start Firecracker process
	if err := vm.client.StartFirecracker(ctx, vm.config.FirecrackerBin, vm.consolePath()); err != nil {
		return fmt.Errorf("failed to start firecracker: %w", err)
	}

	// Configure logging if specified
	if vm.config.LogPath != "" {
		if err := vm.client.SetLogger(ctx, Logger{
			LogPath:       vm.config.LogPath,
			Level:         "Info",
			ShowLevel:     true,
			ShowLogOrigin: true,
		}); err != nil {
			vm.logger.WithError(err).Warn("Failed to configure logging")
		}
	}

	// Configure metrics if specified
	if vm.config.MetricsPath != "" {
		if err := vm.client.SetMetrics(ctx, Metrics{
			MetricsPath: vm.config.MetricsPath,
		}); err != nil {
			vm.logger.WithError(err).Warn("Failed to configure metrics")
		}
	}

	// Set machine config
	if err := vm.client.SetMachineConfig(ctx, MachineConfig{
		VCPUCount:       vm.config.VCPUs,
		MemSizeMib:      vm.config.MemoryMB,
		TrackDirtyPages: true,
	}); err != nil {
		return fmt.Errorf("failed to set machine config: %w", err)
	}

	// Enable virtio-rng entropy device so the guest kernel CRNG initializes
	// immediately. Without this, getrandom() blocks and TLS (git, curl) hangs.
	if err := vm.client.SetEntropyDevice(ctx, EntropyDevice{}); err != nil {
		vm.logger.WithError(err).Warn("Failed to set entropy device (requires Firecracker >= 1.5)")
	}

	// Set boot source
	if err := vm.client.SetBootSource(ctx, BootSource{
		KernelImagePath: vm.config.KernelPath,
		BootArgs:        vm.config.BootArgs,
	}); err != nil {
		return fmt.Errorf("failed to set boot source: %w", err)
	}

	// Add root drive
	if err := vm.client.AddDrive(ctx, Drive{
		DriveID:      "rootfs",
		PathOnHost:   vm.config.RootfsPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		return fmt.Errorf("failed to add root drive: %w", err)
	}

	// Add additional drives
	for _, drive := range vm.config.Drives {
		if err := vm.client.AddDrive(ctx, drive); err != nil {
			return fmt.Errorf("failed to add drive %s: %w", drive.DriveID, err)
		}
	}

	// Configure network interface
	if vm.config.NetworkIface != nil {
		if err := vm.client.AddNetworkInterface(ctx, *vm.config.NetworkIface); err != nil {
			return fmt.Errorf("failed to add network interface: %w", err)
		}
	}

	// Configure vsock
	if vm.config.Vsock != nil {
		if err := vm.client.SetVsock(ctx, *vm.config.Vsock); err != nil {
			return fmt.Errorf("failed to set vsock: %w", err)
		}
	}

	// Configure MMDS
	if vm.config.MMDSConfig != nil {
		if err := vm.client.SetMMDSConfig(ctx, *vm.config.MMDSConfig); err != nil {
			return fmt.Errorf("failed to set MMDS config: %w", err)
		}
	}

	// Start the instance
	if err := vm.client.StartInstance(ctx); err != nil {
		return fmt.Errorf("failed to start instance: %w", err)
	}

	vm.logger.Info("MicroVM started successfully")
	return nil
}

// RestoreFromSnapshot restores the microVM from a snapshot.
//
// The caller must ensure that drive backing files are accessible at the paths
// baked into the snapshot state file. The snapshot-builder creates drives at
// /tmp/snapshot/*.img, so symlinks from those paths to the actual host paths
// must exist before calling this function (see Manager.setupSnapshotSymlinks).
func (vm *VM) RestoreFromSnapshot(ctx context.Context, snapshotPath, memPath string, resume bool) error {
	vm.logger.WithFields(logrus.Fields{
		"snapshot": snapshotPath,
		"mem":      memPath,
	}).Info("Restoring microVM from snapshot")

	// Verify snapshot files exist
	if _, err := os.Stat(snapshotPath); err != nil {
		return fmt.Errorf("snapshot file not found: %w", err)
	}
	if _, err := os.Stat(memPath); err != nil {
		return fmt.Errorf("memory file not found: %w", err)
	}

	// Start Firecracker process
	if err := vm.client.StartFirecracker(ctx, vm.config.FirecrackerBin, vm.consolePath()); err != nil {
		return fmt.Errorf("failed to start firecracker: %w", err)
	}

	// Load snapshot and optionally resume. Firecracker opens drive backing files
	// during LoadSnapshot at the paths recorded in the snapshot state file.
	// These paths must exist (the caller sets up symlinks to handle path differences).
	if err := vm.client.LoadSnapshot(ctx, SnapshotLoadParams{
		SnapshotPath: snapshotPath,
		MemBackend: &MemBackend{
			BackendPath: memPath,
			BackendType: "File",
		},
		EnableDiffSnapshots: true,
		ResumeVM:            resume,
	}); err != nil {
		return fmt.Errorf("failed to load snapshot: %w", err)
	}

	// IMPORTANT: Network interface host_dev_name CANNOT be changed after snapshot load.
	// The TAP device name used when creating the snapshot MUST match the TAP device available
	// on the host when restoring. We use slot-based TAP names (e.g., "tap-slot-0") to ensure
	// consistent naming between snapshot creation and restore.
	if vm.config.NetworkIface != nil {
		vm.logger.WithFields(logrus.Fields{
			"iface_id": vm.config.NetworkIface.IfaceID,
			"host_dev": vm.config.NetworkIface.HostDevName,
		}).Debug("Network interface uses pre-existing TAP device from snapshot")
	}

	// NOTE: MMDS config IS persisted in the snapshot, so we don't need to set it again.
	// After restore, we can PUT/PATCH MMDS data (done by caller after this function returns).

	vm.logger.Info("MicroVM restored from snapshot successfully")
	return nil
}

// CreateSnapshot creates a snapshot of the running microVM
func (vm *VM) CreateSnapshot(ctx context.Context, snapshotPath, memPath string) error {
	vm.logger.WithFields(logrus.Fields{
		"snapshot": snapshotPath,
		"mem":      memPath,
	}).Info("Creating snapshot")

	// Ensure directories exist
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0755); err != nil {
		return fmt.Errorf("failed to create snapshot directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(memPath), 0755); err != nil {
		return fmt.Errorf("failed to create memory directory: %w", err)
	}

	// Pause the VM first
	if err := vm.client.PauseVM(ctx); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	// Create the snapshot
	if err := vm.client.CreateSnapshot(ctx, SnapshotCreateParams{
		SnapshotPath: snapshotPath,
		MemFilePath:  memPath,
		SnapshotType: "Full",
	}); err != nil {
		// Try to resume on failure
		vm.client.ResumeVM(ctx)
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	vm.logger.Info("Snapshot created successfully")
	return nil
}

// CreateDiffSnapshot creates a diff snapshot of the running microVM.
// Diff snapshots only capture dirty pages since the last snapshot or restore,
// resulting in much smaller memory files. Requires track_dirty_pages to have
// been enabled at load time (via EnableDiffSnapshots in SnapshotLoadParams).
func (vm *VM) CreateDiffSnapshot(ctx context.Context, snapshotPath, memDiffPath string) error {
	vm.logger.WithFields(logrus.Fields{
		"snapshot": snapshotPath,
		"mem_diff": memDiffPath,
	}).Info("Creating diff snapshot")

	// Ensure directories exist
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0755); err != nil {
		return fmt.Errorf("failed to create snapshot directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(memDiffPath), 0755); err != nil {
		return fmt.Errorf("failed to create memory directory: %w", err)
	}

	// Pause the VM first
	if err := vm.client.PauseVM(ctx); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	// Create the diff snapshot
	if err := vm.client.CreateSnapshot(ctx, SnapshotCreateParams{
		SnapshotPath: snapshotPath,
		MemFilePath:  memDiffPath,
		SnapshotType: "Diff",
	}); err != nil {
		// Try to resume on failure
		vm.client.ResumeVM(ctx)
		return fmt.Errorf("failed to create diff snapshot: %w", err)
	}

	vm.logger.Info("Diff snapshot created successfully")
	return nil
}

// Pause pauses the microVM
func (vm *VM) Pause(ctx context.Context) error {
	return vm.client.PauseVM(ctx)
}

// Resume resumes the microVM
func (vm *VM) Resume(ctx context.Context) error {
	return vm.client.ResumeVM(ctx)
}

// Stop stops the microVM
func (vm *VM) Stop() error {
	return vm.client.StopFirecracker()
}

// SetMMDSData sets data in the MMDS for the microVM
func (vm *VM) SetMMDSData(ctx context.Context, data interface{}) error {
	return vm.client.PutMMDSData(ctx, data)
}

// UpdateMMDSData patches data in the MMDS
func (vm *VM) UpdateMMDSData(ctx context.Context, data interface{}) error {
	return vm.client.PatchMMDSData(ctx, data)
}

// IsRunning checks if the VM is running
func (vm *VM) IsRunning() bool {
	return vm.client.IsRunning()
}

// RestoreFromSnapshotWithUFFD restores the microVM from a snapshot using UFFD for lazy memory loading.
// The uffdSocketPath should be a Unix socket where a UFFD handler is listening.
// Firecracker will send the UFFD file descriptor to this socket, and the handler will
// service page faults by fetching memory chunks on demand.
func (vm *VM) RestoreFromSnapshotWithUFFD(ctx context.Context, snapshotPath, uffdSocketPath string, resume bool) error {
	vm.logger.WithFields(logrus.Fields{
		"snapshot":    snapshotPath,
		"uffd_socket": uffdSocketPath,
	}).Info("Restoring microVM from snapshot with UFFD backend")

	// Verify snapshot file exists
	if _, err := os.Stat(snapshotPath); err != nil {
		return fmt.Errorf("snapshot file not found: %w", err)
	}

	// Verify UFFD socket exists (handler should be listening)
	if _, err := os.Stat(uffdSocketPath); err != nil {
		return fmt.Errorf("UFFD socket not found: %w", err)
	}

	// Start Firecracker process
	if err := vm.client.StartFirecracker(ctx, vm.config.FirecrackerBin, vm.consolePath()); err != nil {
		return fmt.Errorf("failed to start firecracker: %w", err)
	}

	// Load the snapshot with UFFD backend
	if err := vm.client.LoadSnapshot(ctx, SnapshotLoadParams{
		SnapshotPath: snapshotPath,
		MemBackend: &MemBackend{
			BackendPath: uffdSocketPath,
			BackendType: "Uffd",
		},
		EnableDiffSnapshots: true,
		ResumeVM:            resume,
	}); err != nil {
		return fmt.Errorf("failed to load snapshot with UFFD: %w", err)
	}

	vm.logger.Info("MicroVM restored from snapshot with UFFD successfully")
	return nil
}

// ID returns the VM ID
func (vm *VM) ID() string {
	return vm.config.VMID
}

// Client returns the underlying Firecracker client
func (vm *VM) Client() *Client {
	return vm.client
}
