package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/accessplane"
	"github.com/rahul-roy-glean/capsule/pkg/firecracker"
	"github.com/rahul-roy-glean/capsule/pkg/fuse"
	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
	"github.com/rahul-roy-glean/capsule/pkg/uffd"
)

var errBaseImageNotPinned = errors.New("base image is not digest-pinned")

var (
	gcsBucket      = flag.String("gcs-bucket", "", "GCS bucket for snapshots")
	outputDir      = flag.String("output-dir", "/tmp/snapshot", "Output directory for snapshot files")
	kernelPath     = flag.String("kernel-path", "/opt/firecracker/kernel.bin", "Path to kernel")
	rootfsPath     = flag.String("rootfs-path", "/opt/firecracker/rootfs.img", "Path to base rootfs")
	firecrackerBin = flag.String("firecracker-bin", "/usr/local/bin/firecracker", "Path to firecracker binary")
	vcpus          = flag.Int("vcpus", 4, "vCPUs for warmup VM")
	memoryMB       = flag.Int("memory-mb", 8192, "Memory MB for warmup VM")
	warmupTimeout  = flag.Duration("warmup-timeout", 30*time.Minute, "Timeout for warmup phase")
	rootfsSizeGB   = flag.Int("rootfs-size-gb", 0, "Expand rootfs to this size in GB (0 = keep original size). Increase if bazel fetch runs out of space.")
	logLevel       = flag.String("log-level", "info", "Log level")
	memBackend     = flag.String("mem-backend", "chunked", "Memory backend for chunked snapshots: 'chunked' (UFFD lazy loading via MemChunks, default) or 'file' (upload snapshot.mem as single blob)")
	gcsPrefix      = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")

	versionOverride = flag.String("version", "", "Snapshot version string (if empty, auto-generated from timestamp + workload key)")

	// Snapshot commands (replaces --repo-slug/--repo-url/--repo-branch/--bazel-version)
	snapshotCommands = flag.String("snapshot-commands", "", "JSON array of SnapshotCommand describing what to bake into the snapshot (required)")

	// Layer build flags
	layerHash            = flag.String("layer-hash", "", "Layer hash for metadata tagging")
	parentWorkloadKey    = flag.String("parent-workload-key", "", "Parent layer's hash, used as GCS workload key to load parent snapshot")
	parentVersion        = flag.String("parent-version", "", "Version of parent snapshot to restore from")
	layerDrives          = flag.String("layer-drives", "", "JSON array of DriveSpec for this layer's new extension drives")
	buildType            = flag.String("build-type", "init", "Build type: init, refresh, or reattach")
	previousLayerKey     = flag.String("previous-layer-key", "", "Old layer hash for loading extension drives during reattach")
	previousLayerVersion = flag.String("previous-layer-version", "", "Version of old layer to load drives from")

	// Base image support: build rootfs from Docker image instead of pre-baked rootfs.img
	baseImage  = flag.String("base-image", "", "Docker image URI to use as rootfs base (e.g. 'ubuntu:22.04'). When set, pulls the image, converts to ext4, and installs capsule-thaw-agent.")
	runnerUser = flag.String("runner-user", "runner", "Username for non-root commands inside the VM")

	// Auth proxy: transparent credential injection for the warmup VM
	authConfigJSON = flag.String("auth-config", "", "JSON access plane config (accessplane.Config). When set, injects access plane proxy info into MMDS for the build VM.")

	// Path to a pre-built capsule-thaw-agent binary to inject into the rootfs.
	// If empty or the file does not exist, snapshot-builder will attempt to
	// build capsule-thaw-agent from source (requires Go toolchain on the host).
	thawAgentPath = flag.String("capsule-thaw-agent-path", "/usr/local/bin/capsule-thaw-agent", "Path to capsule-thaw-agent binary (if missing, builds from source)")
)

func main() {
	flag.Parse()

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	if err := run(logger); err != nil {
		logger.WithField("component", "snapshot-builder").WithError(err).Error("Snapshot build failed")
		os.Exit(1)
	}
}

func run(logger *logrus.Logger) error {
	// Setup logger state for this invocation

	log := logger.WithField("component", "snapshot-builder")
	log.Info("Starting snapshot builder")

	if *snapshotCommands == "" {
		return errors.New("--snapshot-commands is required")
	}
	if *gcsBucket == "" {
		return errors.New("--gcs-bucket is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *warmupTimeout+30*time.Minute)
	defer cancel()

	// Get GCP access token from metadata server (for Artifact Registry auth inside warmup VM)
	gcpAccessToken := ""
	if resp, err := http.Get("http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"); err == nil {
		// Metadata server requires Metadata-Flavor header, retry with it
		resp.Body.Close()
	}
	metadataReq, _ := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	metadataReq.Header.Set("Metadata-Flavor", "Google")
	if resp, err := http.DefaultClient.Do(metadataReq); err == nil {
		defer resp.Body.Close()
		var tokenResp struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
			TokenType   string `json:"token_type"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err == nil && tokenResp.AccessToken != "" {
			gcpAccessToken = tokenResp.AccessToken
			log.WithField("expires_in", tokenResp.ExpiresIn).Info("Obtained GCP access token for Artifact Registry auth")
		}
	} else {
		log.WithError(err).Warn("Failed to get GCP access token (Artifact Registry auth won't work in warmup VM)")
	}

	// Parse snapshot commands early — needed for workload key and version.
	var commands []snapshot.SnapshotCommand
	if err := json.Unmarshal([]byte(*snapshotCommands), &commands); err != nil {
		return fmt.Errorf("invalid --snapshot-commands: %w", err)
	}
	if len(commands) == 0 {
		return errors.New("--snapshot-commands must be non-empty")
	}
	workloadKey := snapshot.ComputeWorkloadKey(commands)
	log.WithField("workload_key", workloadKey).Info("Computed workload key from snapshot commands")

	// Layer mode: use layer_hash as GCS directory key
	effectiveWorkloadKey := resolveSnapshotLookupKey(workloadKey, *layerHash)
	if effectiveWorkloadKey != workloadKey {
		log.WithFields(logrus.Fields{
			"layer_hash":             *layerHash,
			"effective_workload_key": effectiveWorkloadKey,
		}).Info("Layer mode: using layer_hash as GCS key")
	}

	// Parse layer drives if provided
	var newDrives []snapshot.DriveSpec
	if *layerDrives != "" {
		if err := json.Unmarshal([]byte(*layerDrives), &newDrives); err != nil {
			log.WithError(err).Warn("invalid --layer-drives, ignoring")
		}
	}

	// For refresh/reattach builds, use the incremental restore path.
	// Also use it for init builds that have a parent layer (child init builds
	// should restore from parent snapshot, not cold boot).
	incremental := *buildType == "refresh" || *buildType == "reattach"
	if !incremental && *parentWorkloadKey != "" && *parentVersion != "" {
		incremental = true
		log.Info("Init build with parent layer: will restore from parent snapshot")
	}
	if incremental {
		log.WithField("build_type", *buildType).Info("Using incremental restore path")
	}
	if err := validateBaseImagePolicy(incremental, *baseImage); err != nil {
		return err
	}

	// Generate version string (use override from control plane if provided)
	version := *versionOverride
	if version == "" {
		version = fmt.Sprintf("v%s-%s", time.Now().Format("20060102-150405"), effectiveWorkloadKey[:16])
	}
	log.WithField("version", version).Info("Building snapshot")

	// Create output directory
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// Network constants shared by both cold boot and incremental paths
	vmID := "snapshot-builder"
	socketPath := filepath.Join(*outputDir, vmID+".sock")

	cleanupSocket := func() {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			log.WithError(err).WithField("path", socketPath).Warn("Failed to remove snapshot-builder socket")
		}
	}
	defer cleanupSocket()

	log.Info("Setting up network for warmup VM...")
	builderNet, err := setupBuilderNetwork(vmID, logger)
	if err != nil {
		return fmt.Errorf("setup builder network: %w", err)
	}
	defer func() {
		if err := builderNet.cleanup(); err != nil {
			log.WithError(err).Warn("Failed to cleanup builder network")
		}
	}()
	tapName := builderNet.tapName()     // Must match snapshot contract inside namespace
	guestIP := builderNet.guestIP()     // Slot 0 guest IP remains 172.16.0.2
	guestMAC := builderNet.guestMAC()   // Deterministic MAC from tap allocation
	pollIP := builderNet.pollIP()       // Host-reachable IP for capsule-thaw-agent polling
	gatewayIP := builderNet.gatewayIP() // Guest-visible gateway, typically 172.16.0.1
	netmask := builderNet.netmask()     // Guest-visible netmask
	firecrackerNetNSPath := builderNet.firecrackerNetNSPath()

	// Parse access plane config if provided.
	// The access plane proxy is assumed to be running externally; we just inject
	// its connection info into MMDS so the build VM can reach it.
	var accessPlaneConfig *accessplane.Config
	if *authConfigJSON != "" {
		var cfg accessplane.Config
		if err := json.Unmarshal([]byte(*authConfigJSON), &cfg); err != nil {
			return fmt.Errorf("parse --auth-config JSON: %w", err)
		}
		accessPlaneConfig = &cfg
		log.WithFields(logrus.Fields{
			"proxy_endpoint": cfg.ProxyEndpoint,
			"api_endpoint":   cfg.APIEndpoint,
			"tenant_id":      cfg.TenantID,
		}).Info("Access plane config loaded")
	} else {
		log.Warn("No --auth-config provided, access plane disabled (git clone of private repos will fail)")
	}

	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init ip=%s::%s:%s::eth0:off",
		guestIP, gatewayIP, netmask)

	// Track FUSE disks for cleanup and incremental chunking
	var vm *firecracker.VM
	var fuseDisk *fuse.ChunkedDisk
	var fuseExtDisks map[string]*fuse.ChunkedDisk // driveID → FUSE disk for extension drives
	var incrUffdHandler *uffd.Handler
	stopVM := func(reason string) {
		if vm == nil {
			return
		}
		if err := vm.Stop(); err != nil {
			log.WithError(err).WithField("reason", reason).Warn("Failed to stop VM during cleanup")
		}
		vm = nil
	}
	defer stopVM("run exit")

	stopUFFD := func(reason string) {
		if incrUffdHandler == nil {
			return
		}
		log.WithField("reason", reason).Debug("Stopping UFFD handler during cleanup")
		incrUffdHandler.Stop()
		incrUffdHandler = nil
	}
	defer stopUFFD("run exit")

	unmountRootfsFUSE := func(reason string) {
		if fuseDisk == nil {
			return
		}
		if err := fuseDisk.Unmount(); err != nil {
			log.WithError(err).WithField("reason", reason).Warn("Failed to unmount rootfs FUSE disk during cleanup")
		}
		fuseDisk = nil
	}
	defer unmountRootfsFUSE("run exit")

	unmountExtensionFUSE := func(reason string) {
		for driveID, d := range fuseExtDisks {
			if err := d.Unmount(); err != nil {
				log.WithError(err).WithFields(logrus.Fields{
					"reason":   reason,
					"drive_id": driveID,
				}).Warn("Failed to unmount extension FUSE disk during cleanup")
			}
		}
		fuseExtDisks = nil
	}
	defer unmountExtensionFUSE("run exit")

	// Paths used by both paths and for final snapshot creation
	workingRootfs := filepath.Join(*outputDir, "rootfs.img")

	// rootfsSourceHash is lazily computed from the effective rootfs provenance
	// inputs and stored in chunked metadata so future restores can detect when
	// they would resume against stale rootfs content.
	var rootfsSourceHash string
	var rootfsFlavorForProvenance rootfsFlavor

	// expectedRunnerID is the runner_id we expect to see in warmup status.
	// For reattach/incremental builds (restoring from parent snapshot), we set
	// this to the new runner_id so waitForWarmup ignores stale status from the
	// parent layer's completed warmup.
	var expectedRunnerID string

	// Try incremental/reattach restore from previous snapshot
	if incremental {
		expectedRunnerID = fmt.Sprintf("snapshot-builder-restore-%s", uuid.New().String()[:8])
		if *parentWorkloadKey != "" && *parentVersion != "" {
			// Restore from parent layer: parent's VM state (+ old layer's extension drives if reattach)
			log.Info("Attempting restore from parent layer...")
			var reattachErr error
			vm, fuseDisk, fuseExtDisks, incrUffdHandler, rootfsFlavorForProvenance, reattachErr = reattachFromParent(ctx, logger, log, vmID, tapName, guestMAC, bootArgs, firecrackerNetNSPath, gcpAccessToken, commands, newDrives, expectedRunnerID, accessPlaneConfig)
			if reattachErr != nil {
				// Child layers MUST restore from parent — cold boot would produce
				// a fundamentally different snapshot (wrong rootfs, missing parent state).
				// Fail the build so it can be retried instead of silently degrading.
				if incrUffdHandler != nil {
					incrUffdHandler.Stop()
				}
				if fuseDisk != nil {
					fuseDisk.Unmount()
				}
				for _, d := range fuseExtDisks {
					d.Unmount()
				}
				return fmt.Errorf("parent layer restore failed (cold boot not allowed for child layers): %w", reattachErr)
			}
		} else {
			log.Info("Attempting incremental restore from previous snapshot...")
			var incrementalErr error
			vm, fuseDisk, fuseExtDisks, incrUffdHandler, rootfsFlavorForProvenance, incrementalErr = restoreFromPreviousSnapshot(ctx, logger, log, vmID, tapName, guestMAC, bootArgs, firecrackerNetNSPath, effectiveWorkloadKey, gcpAccessToken, commands, newDrives, expectedRunnerID, accessPlaneConfig)
			if incrementalErr != nil {
				log.WithError(incrementalErr).Warn("Incremental restore failed, falling back to cold boot")
				vm = nil
				expectedRunnerID = "" // Cold boot fallback — no stale state
				if incrUffdHandler != nil {
					incrUffdHandler.Stop()
					incrUffdHandler = nil
				}
				if fuseDisk != nil {
					fuseDisk.Unmount()
					fuseDisk = nil
				}
				for _, d := range fuseExtDisks {
					d.Unmount()
				}
				fuseExtDisks = nil
			}
		}
	}

	// Cold boot path (default or fallback from failed incremental)
	if vm == nil {
		log.Info("Using cold boot path...")

		if *baseImage != "" {
			// Build rootfs from Docker image: pull → export → inject platform shim → ext4
			log.WithField("base_image", *baseImage).Info("Building rootfs from Docker image...")
			var buildFlavor rootfsFlavor
			buildFlavor, err = buildRootfsFromImage(*baseImage, workingRootfs, *runnerUser, log)
			if err != nil {
				return fmt.Errorf("build rootfs from Docker image: %w", err)
			}
			rootfsFlavorForProvenance = buildFlavor
		} else {
			// Legacy path: copy pre-baked rootfs.img
			log.Info("Creating working rootfs from pre-baked image...")
			if err := copyFile(*rootfsPath, workingRootfs); err != nil {
				return fmt.Errorf("copy rootfs: %w", err)
			}
		}
		if *rootfsSizeGB > 0 {
			log.WithField("size_gb", *rootfsSizeGB).Info("Expanding rootfs...")
			if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", *rootfsSizeGB), workingRootfs).Run(); err != nil {
				return fmt.Errorf("expand rootfs: %w", err)
			}
			if output, err := exec.Command("e2fsck", "-fy", workingRootfs).CombinedOutput(); err != nil {
				log.WithField("output", string(output)).Warn("e2fsck returned non-zero (may be OK)")
			}
			if output, err := exec.Command("resize2fs", workingRootfs).CombinedOutput(); err != nil {
				return fmt.Errorf("resize2fs rootfs: %s", string(output))
			}
			log.Info("Rootfs expanded successfully")
		}

		// Build the list of extension drives for the VM.
		// Layer builds: create full-size sparse drives for all drives defined
		// across ALL layers in the config (the "chain union"). Every layer gets
		// the same set of drives so that Firecracker snapshot restore works —
		// you can't add/remove drives between snapshot and restore. Sparse files
		// don't consume actual disk space until written to.
		var drives []firecracker.Drive

		if *layerHash != "" {
			// Layer mode: create sparse drives for all config-defined drives.
			log.WithField("num_drives", len(newDrives)).Info("Layer mode: creating sparse drives for all chain drives")
			for _, d := range newDrives {
				imgPath := filepath.Join(*outputDir, d.DriveID+".img")
				sizeGB := d.SizeGB
				if sizeGB <= 0 {
					sizeGB = 50
				}
				label := d.Label
				if label == "" {
					label = strings.ToUpper(d.DriveID)
				}
				if err := createExt4Image(imgPath, sizeGB, label); err != nil {
					return fmt.Errorf("create drive image %s: %w", d.DriveID, err)
				}
				drives = append(drives, firecracker.Drive{
					DriveID:      d.DriveID,
					PathOnHost:   imgPath,
					IsRootDevice: false,
					IsReadOnly:   d.ReadOnly,
				})
				log.WithFields(logrus.Fields{
					"drive_id": d.DriveID,
					"size_gb":  sizeGB,
					"label":    label,
				}).Info("Created sparse drive")
			}
		}

		vmCfg := firecracker.VMConfig{
			VMID:           vmID,
			SocketDir:      *outputDir,
			FirecrackerBin: *firecrackerBin,
			KernelPath:     *kernelPath,
			RootfsPath:     workingRootfs,
			VCPUs:          *vcpus,
			MemoryMB:       *memoryMB,
			BootArgs:       bootArgs,
			LogPath:        filepath.Join(*outputDir, vmID+".log"),
			NetworkIface: &firecracker.NetworkInterface{
				IfaceID:     "eth0",
				HostDevName: tapName,
				GuestMAC:    guestMAC,
			},
			MMDSConfig: &firecracker.MMDSConfig{
				Version:           "V1",
				NetworkInterfaces: []string{"eth0"},
			},
			Drives:    drives,
			NetNSPath: firecrackerNetNSPath,
		}

		var err error
		vm, err = firecracker.NewVM(vmCfg, logger)
		if err != nil {
			return fmt.Errorf("create VM: %w", err)
		}

		log.Info("Starting warmup VM...")
		if err := vm.Start(ctx); err != nil {
			return fmt.Errorf("start VM: %w", err)
		}

		mmdsData := buildWarmupMMDS(commands, newDrives)
		injectProxyMMDS(mmdsData, accessPlaneConfig)
		if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
			stopVM("set MMDS data failure")
			return fmt.Errorf("set MMDS data: %w", err)
		}
	}

	// Shared path: wait for warmup, snapshot, upload
	log.Info("Waiting for warmup to complete...")
	warmupCtx, warmupCancel := context.WithTimeout(ctx, *warmupTimeout)
	defer warmupCancel()

	if err := waitForWarmup(warmupCtx, vm, pollIP, expectedRunnerID, log); err != nil {
		stopVM("warmup failure")
		return fmt.Errorf("warmup failed: %w", err)
	}

	// Create snapshot
	log.Info("Creating snapshot...")
	snapshotPath := filepath.Join(*outputDir, "snapshot.state")
	memPath := filepath.Join(*outputDir, "snapshot.mem")

	if err := vm.CreateSnapshot(ctx, snapshotPath, memPath); err != nil {
		stopVM("snapshot creation failure")
		return fmt.Errorf("create snapshot: %w", err)
	}

	// Stop VM
	stopVM("snapshot created")

	// Stop UFFD handler (if used for incremental restore)
	stopUFFD("snapshot created")

	// If we used FUSE-backed rootfs (incremental), save dirty chunks to the chunk store.
	// This uploads only the changed chunks. We store the updated chunk refs for use
	// when building the chunked metadata (avoids re-reading the entire rootfs).
	var incrementalRootfsChunks []snapshot.ChunkRef
	incrementalExtChunks := make(map[string][]snapshot.ChunkRef) // driveID → chunks
	wasIncremental := fuseDisk != nil

	if fuseDisk != nil {
		log.WithField("dirty_chunks", fuseDisk.DirtyChunkCount()).Info("Saving FUSE rootfs dirty chunks to chunk store...")
		var err error
		incrementalRootfsChunks, err = fuseDisk.SaveDirtyChunks(ctx)
		if err != nil {
			return fmt.Errorf("save dirty rootfs chunks: %w", err)
		}
		log.WithFields(logrus.Fields{
			"total_chunks": len(incrementalRootfsChunks),
			"dirty_chunks": fuseDisk.DirtyChunkCount(),
		}).Info("Rootfs dirty chunks saved to chunk store")
		unmountRootfsFUSE("rootfs dirty chunks saved")
	}
	for driveID, extDisk := range fuseExtDisks {
		log.WithFields(logrus.Fields{
			"drive_id":     driveID,
			"dirty_chunks": extDisk.DirtyChunkCount(),
		}).Info("Saving FUSE extension drive dirty chunks to chunk store...")
		chunks, err := extDisk.SaveDirtyChunks(ctx)
		if err != nil {
			return fmt.Errorf("save dirty extension drive chunks for %s: %w", driveID, err)
		}
		incrementalExtChunks[driveID] = chunks
	}
	unmountExtensionFUSE("extension drive dirty chunks saved")

	// Copy kernel to output
	kernelOutput := filepath.Join(*outputDir, "kernel.bin")
	if wasIncremental {
		// Incremental/reattach: kernel was downloaded to a subdir, copy to output.
		// Check both "incremental" (self-refresh) and "reattach" (parent-based) paths.
		incrKernel := filepath.Join(*outputDir, "incremental", "kernel.bin")
		reattachKernel := filepath.Join(*outputDir, "reattach", "kernel.bin")
		var kernelSrc string
		if _, err := os.Stat(incrKernel); err == nil {
			kernelSrc = incrKernel
		} else if _, err := os.Stat(reattachKernel); err == nil {
			kernelSrc = reattachKernel
		} else {
			return errors.New("failed to find kernel in incremental or reattach dir")
		}
		if err := copyFile(kernelSrc, kernelOutput); err != nil {
			return fmt.Errorf("copy kernel from restore dir: %w", err)
		}
	} else {
		// Cold boot: copy kernel from flag path
		if err := copyFile(*kernelPath, kernelOutput); err != nil {
			return fmt.Errorf("copy kernel: %w", err)
		}
	}

	// Get file sizes (some files may not exist in incremental mode)
	var totalSize int64
	for _, f := range []string{kernelOutput, workingRootfs, snapshotPath, memPath} {
		info, _ := os.Stat(f)
		if info != nil {
			totalSize += info.Size()
		}
	}

	// Upload to GCS
	uploader, err := snapshot.NewUploader(ctx, snapshot.UploaderConfig{
		GCSBucket: *gcsBucket,
		GCSPrefix: *gcsPrefix,
		Logger:    logger,
	})
	if err != nil {
		return fmt.Errorf("create uploader: %w", err)
	}
	defer uploader.Close()

	// Build and upload chunked snapshot for lazy loading
	log.Info("Building chunked snapshot for lazy loading...")

	chunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
		GCSBucket:      *gcsBucket,
		GCSPrefix:      *gcsPrefix,
		LocalCachePath: filepath.Join(*outputDir, "chunk-cache"),
		ChunkSubdir:    "disk",
		Logger:         logger,
	})
	if err != nil {
		return fmt.Errorf("create chunk store: %w", err)
	}
	defer chunkStore.Close()

	memChunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
		GCSBucket:      *gcsBucket,
		GCSPrefix:      *gcsPrefix,
		LocalCachePath: filepath.Join(*outputDir, "chunk-cache"),
		ChunkSubdir:    "mem",
		Logger:         logger,
	})
	if err != nil {
		return fmt.Errorf("create mem chunk store: %w", err)
	}
	defer memChunkStore.Close()

	builder := snapshot.NewChunkedSnapshotBuilder(chunkStore, memChunkStore, logger)
	builder.MemBackend = *memBackend

	// Compute rootfs source hash for storing in metadata (enables incremental change detection).
	// Only needed at upload time — the incremental path hashes independently for comparison.
	if rootfsSourceHash == "" {
		provenanceFlavor := rootfsFlavor("")
		if *baseImage != "" {
			provenanceFlavor = rootfsFlavorForProvenance
		}
		if h, err := computeRootfsSourceHash(*rootfsPath, *baseImage, *runnerUser, *thawAgentPath, *rootfsSizeGB, provenanceFlavor); err == nil {
			rootfsSourceHash = h
			log.WithField("hash", rootfsSourceHash[:12]).Info("Rootfs provenance hash computed for metadata")
		} else if errors.Is(err, errBaseImageNotPinned) {
			log.WithField("base_image", *baseImage).Warn("Base image is not digest-pinned; future incremental restores will fall back to cold boot")
		} else {
			log.WithError(err).Warn("Failed to compute rootfs provenance hash, future incremental builds won't detect rootfs changes")
		}
	}

	if wasIncremental && incrementalRootfsChunks != nil {
		// Incremental: rootfs chunks already in chunk store (original + dirty).
		// Only chunk kernel, state, and mem (which are new). Skip rootfs chunking.
		log.Info("Incremental chunked snapshot: reusing rootfs chunks, chunking mem/state/kernel...")

		chunkedMeta := &snapshot.ChunkedSnapshotMetadata{
			Version:          version,
			WorkloadKey:      effectiveWorkloadKey,
			Commands:         commands,
			CreatedAt:        time.Now(),
			ChunkSize:        snapshot.DefaultChunkSize,
			RootfsSourceHash: rootfsSourceHash,
			RootfsFlavor:     string(rootfsFlavorForProvenance),
		}
		// Populate layer fields if in layer mode
		if *layerHash != "" {
			chunkedMeta.LayerHash = *layerHash
			chunkedMeta.ParentLayerHash = *parentWorkloadKey
			chunkedMeta.ParentVersion = *parentVersion
		}

		// Chunk kernel
		kernelData, err := os.ReadFile(kernelOutput)
		if err != nil {
			return fmt.Errorf("read kernel for chunking: %w", err)
		}
		kernelHash, _, err := chunkStore.StoreChunk(ctx, kernelData)
		if err != nil {
			return fmt.Errorf("store kernel chunk: %w", err)
		}
		chunkedMeta.KernelHash = kernelHash

		// Chunk state
		stateData, err := os.ReadFile(snapshotPath)
		if err != nil {
			return fmt.Errorf("read state for chunking: %w", err)
		}
		stateHash, _, err := chunkStore.StoreChunk(ctx, stateData)
		if err != nil {
			return fmt.Errorf("store state chunk: %w", err)
		}
		chunkedMeta.StateHash = stateHash

		// Store memory according to --mem-backend flag.
		if *memBackend == "file" {
			memGCSPath := fmt.Sprintf("%s/snapshot_state/%s/snapshot.mem.zst", effectiveWorkloadKey, version)
			_, _, err = memChunkStore.UploadRawFile(ctx, memPath, memGCSPath)
			if err != nil {
				return fmt.Errorf("upload raw memory file: %w", err)
			}
			chunkedMeta.MemFilePath = memGCSPath
		} else {
			memChunks, err := memChunkStore.ChunkFile(ctx, memPath, snapshot.DefaultChunkSize)
			if err != nil {
				return fmt.Errorf("chunk memory file: %w", err)
			}
			chunkedMeta.MemChunks = memChunks
		}
		memStat, _ := os.Stat(memPath)
		if memStat != nil {
			chunkedMeta.TotalMemSize = memStat.Size()
		}
		// Use rootfs chunks from FUSE (original + dirty writes already uploaded)
		chunkedMeta.RootfsChunks = incrementalRootfsChunks
		chunkedMeta.TotalDiskSize = int64(len(incrementalRootfsChunks)) * chunkedMeta.ChunkSize

		// Add extension drives that were FUSE-tracked (dirty chunks already saved)
		if len(incrementalExtChunks) > 0 {
			if chunkedMeta.ExtensionDrives == nil {
				chunkedMeta.ExtensionDrives = make(map[string]snapshot.ExtensionDrive)
			}
			// Build a lookup from driveID → DriveSpec so FUSE-tracked drives
			// can inherit Label/MountPath for MMDS propagation.
			driveSpecByID := make(map[string]snapshot.DriveSpec, len(newDrives))
			for _, d := range newDrives {
				driveSpecByID[d.DriveID] = d
			}
			for driveID, chunks := range incrementalExtChunks {
				var totalSize int64
				for _, c := range chunks {
					if end := c.Offset + c.Size; end > totalSize {
						totalSize = end
					}
				}
				spec := driveSpecByID[driveID]
				chunkedMeta.ExtensionDrives[driveID] = snapshot.ExtensionDrive{
					Chunks:    chunks,
					ReadOnly:  false,
					SizeBytes: totalSize,
					Label:     spec.Label,
					MountPath: spec.MountPath,
				}
			}
		}

		// Chunk any remaining extension drives from newDrives that weren't
		// FUSE-tracked (e.g. freshly created drives for new layer configs).
		for _, d := range newDrives {
			if _, ok := chunkedMeta.ExtensionDrives[d.DriveID]; ok {
				continue // already handled via FUSE dirty chunks
			}
			imgPath := filepath.Join(*outputDir, d.DriveID+".img")
			if _, err := os.Stat(imgPath); err != nil {
				// Try reattach subdir
				imgPath = filepath.Join(*outputDir, "reattach", d.DriveID+".img")
				if _, err := os.Stat(imgPath); err != nil {
					log.WithField("drive_id", d.DriveID).Warn("Extension drive image not found, skipping")
					continue
				}
			}
			log.WithFields(logrus.Fields{
				"drive_id": d.DriveID,
				"path":     imgPath,
			}).Info("Chunking extension drive from disk")
			chunks, chunkErr := chunkStore.ChunkFile(ctx, imgPath, snapshot.DefaultChunkSize)
			if chunkErr != nil {
				return fmt.Errorf("chunk extension drive %s: %w", d.DriveID, chunkErr)
			}
			if chunkedMeta.ExtensionDrives == nil {
				chunkedMeta.ExtensionDrives = make(map[string]snapshot.ExtensionDrive)
			}
			stat, _ := os.Stat(imgPath)
			chunkedMeta.ExtensionDrives[d.DriveID] = snapshot.ExtensionDrive{
				Chunks:    chunks,
				ReadOnly:  d.ReadOnly,
				SizeBytes: stat.Size(),
				Label:     d.Label,
				MountPath: d.MountPath,
			}
		}

		if err := builder.UploadChunkedMetadata(ctx, chunkedMeta); err != nil {
			return fmt.Errorf("upload incremental chunked metadata: %w", err)
		}

		log.WithFields(logrus.Fields{
			"mem_file":         chunkedMeta.MemFilePath,
			"disk_chunks":      len(chunkedMeta.RootfsChunks),
			"extension_drives": len(chunkedMeta.ExtensionDrives),
		}).Info("Incremental chunked snapshot built and uploaded")
	} else {
		// Cold boot: chunk everything from full files on disk.
		// Include all drives so child layers can restore them via FUSE
		// for filesystem consistency.
		driveSpecs := []snapshot.DriveSpec{}
		driveImages := map[string]string{}

		if *layerHash != "" && len(newDrives) > 0 {
			// Layer mode: include config-defined drives
			for _, d := range newDrives {
				imgPath := filepath.Join(*outputDir, d.DriveID+".img")
				if _, err := os.Stat(imgPath); err == nil {
					driveSpecs = append(driveSpecs, d)
					driveImages[d.DriveID] = imgPath
				}
			}
		}

		snapshotPaths := &snapshot.SnapshotPaths{
			Kernel:               kernelOutput,
			Rootfs:               workingRootfs,
			Mem:                  memPath,
			State:                snapshotPath,
			Version:              version,
			ExtensionDriveImages: driveImages,
		}

		chunkedMeta, err := builder.BuildChunkedSnapshot(ctx, snapshotPaths, driveSpecs, version, effectiveWorkloadKey)
		if err != nil {
			return fmt.Errorf("build chunked snapshot: %w", err)
		}
		chunkedMeta.RootfsSourceHash = rootfsSourceHash
		chunkedMeta.RootfsFlavor = string(rootfsFlavorForProvenance)
		chunkedMeta.Commands = commands
		// Populate layer fields if in layer mode
		if *layerHash != "" {
			chunkedMeta.LayerHash = *layerHash
			chunkedMeta.ParentLayerHash = *parentWorkloadKey
			chunkedMeta.ParentVersion = *parentVersion
		}

		if err := builder.UploadChunkedMetadata(ctx, chunkedMeta); err != nil {
			return fmt.Errorf("upload chunked metadata: %w", err)
		}

		log.WithFields(logrus.Fields{
			"mem_chunks":       len(chunkedMeta.MemChunks),
			"disk_chunks":      len(chunkedMeta.RootfsChunks),
			"extension_drives": len(chunkedMeta.ExtensionDrives),
		}).Info("Chunked snapshot built and uploaded")
	}

	// Update current pointer (workload-key-scoped) — done last so the pointer
	// only moves after all snapshot data has been fully uploaded.
	log.Info("Updating current pointer...")
	if err := uploader.UpdateCurrentPointerForRepo(ctx, version, effectiveWorkloadKey); err != nil {
		return fmt.Errorf("update current pointer: %w", err)
	}

	log.WithFields(logrus.Fields{
		"version":    version,
		"size_bytes": totalSize,
		"gcs_path":   fmt.Sprintf("gs://%s/%s/", *gcsBucket, version),
	}).Info("Snapshot build complete!")

	return nil
}

func validateBaseImagePolicy(incremental bool, baseImage string) error {
	if !incremental || baseImage == "" {
		return nil
	}
	if _, err := normalizePinnedBaseImage(baseImage); err != nil {
		return fmt.Errorf("incremental base-image builds require digest-pinned image references: %w", err)
	}
	return nil
}

func waitForWarmup(ctx context.Context, vm *firecracker.VM, guestIP string, expectedRunnerID string, log *logrus.Entry) error {
	// Wait for capsule-thaw-agent health endpoint to become available
	// The capsule-thaw-agent runs warmup and exposes /health and /warmup-status endpoints

	log.WithFields(logrus.Fields{"guest_ip": guestIP, "expected_runner_id": expectedRunnerID}).Info("Waiting for warmup to complete...")

	healthURL := fmt.Sprintf("http://%s:%d/health", guestIP, snapshot.ThawAgentHealthPort)
	warmupURL := fmt.Sprintf("http://%s:%d/warmup-status", guestIP, snapshot.ThawAgentHealthPort)
	logsURL := fmt.Sprintf("http://%s:%d/warmup-logs", guestIP, snapshot.ThawAgentHealthPort)

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 2 * time.Second,
			}).DialContext,
		},
	}

	// Phase 1: Wait for VM to boot and capsule-thaw-agent to start
	log.Info("Phase 1: Waiting for capsule-thaw-agent to become responsive...")
	bootCtx, bootCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer bootCancel()

	if err := waitForHealth(bootCtx, client, healthURL, log); err != nil {
		return fmt.Errorf("VM failed to boot: %w", err)
	}
	log.Info("Thaw-agent is responsive")

	// Phase 2: Wait for warmup to complete
	log.Info("Phase 2: Waiting for warmup to complete...")

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	lastPhase := ""
	var logSeq int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Poll warmup logs and print new lines
			fetchWarmupLogs(client, logsURL, &logSeq, log)

			status, err := getWarmupStatus(client, warmupURL)
			if err != nil {
				// Warmup endpoint might not exist yet, check health instead
				if _, healthErr := checkHealth(client, healthURL); healthErr != nil {
					log.WithError(healthErr).Warn("Lost connection to VM")
				}
				continue
			}

			if status.Phase != lastPhase {
				log.WithFields(logrus.Fields{
					"phase":   status.Phase,
					"message": status.Message,
				}).Info("Warmup progress")
				lastPhase = status.Phase
			}

			// Ignore stale status from a previous warmup (e.g. parent layer).
			// The capsule-thaw-agent tags warmup status with the runner_id so we can
			// detect when it hasn't yet picked up the new MMDS data.
			if expectedRunnerID != "" && status.RunnerID != expectedRunnerID {
				log.WithFields(logrus.Fields{
					"status_runner_id":   status.RunnerID,
					"expected_runner_id": expectedRunnerID,
					"status_phase":       status.Phase,
					"status_complete":    status.Complete,
				}).Info("Waiting for capsule-thaw-agent to detect new runner_id (stale status from parent)")
				continue
			}

			if status.Complete {
				// Final log flush
				fetchWarmupLogs(client, logsURL, &logSeq, log)
				log.WithFields(logrus.Fields{
					"duration":  status.Duration,
					"externals": status.ExternalsFetched,
				}).Info("Warmup completed successfully")
				return nil
			}

			if status.Error != "" {
				// Final log flush
				fetchWarmupLogs(client, logsURL, &logSeq, log)
				return fmt.Errorf("warmup failed: %s", status.Error)
			}
		}
	}
}

func waitForHealth(ctx context.Context, client *http.Client, healthURL string, log *logrus.Entry) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := checkHealth(client, healthURL); err == nil {
				return nil
			}
			log.Debug("Waiting for capsule-thaw-agent...")
		}
	}
}

func checkHealth(client *http.Client, url string) (map[string]interface{}, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health check returned %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result, nil
}

// WarmupStatus represents the warmup status from capsule-thaw-agent
type WarmupStatus struct {
	Complete         bool   `json:"complete"`
	Phase            string `json:"phase"`
	Message          string `json:"message,omitempty"`
	Error            string `json:"error,omitempty"`
	Duration         string `json:"duration,omitempty"`
	ExternalsFetched int    `json:"externals_fetched,omitempty"`
	RunnerID         string `json:"runner_id,omitempty"`
}

func getWarmupStatus(client *http.Client, url string) (*WarmupStatus, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("warmup status returned %d: %s", resp.StatusCode, string(body))
	}

	var status WarmupStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}

	return &status, nil
}

func fetchWarmupLogs(client *http.Client, baseURL string, seq *int64, log *logrus.Entry) {
	url := fmt.Sprintf("%s?after=%d", baseURL, *seq)
	resp, err := client.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var result struct {
		Lines []string `json:"lines"`
		Seq   int64    `json:"seq"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}

	for _, line := range result.Lines {
		log.WithField("vm", "warmup").Info(line)
	}
	*seq = result.Seq
}

func copyFile(src, dst string) error {
	cmd := exec.Command("cp", "--sparse=always", src, dst)
	return cmd.Run()
}

func resolveSnapshotLookupKey(workloadKey, layerHash string) string {
	if layerHash != "" {
		return layerHash
	}
	return workloadKey
}

// hashFile computes SHA-256 of a file and returns hex string.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func computeRootfsSourceHash(rootfsPath, baseImage, runnerUser, thawAgentPath string, rootfsSizeGB int, flavor rootfsFlavor) (string, error) {
	var payload any
	if baseImage != "" {
		pinnedBaseImage, err := normalizePinnedBaseImage(baseImage)
		if err != nil {
			return "", err
		}
		if flavor == "" {
			return "", fmt.Errorf("rootfs flavor is required for base-image provenance")
		}
		thawAgentHash, err := hashFile(thawAgentPath)
		if err != nil {
			return "", fmt.Errorf("hash capsule-thaw-agent binary: %w", err)
		}
		platformShimHash, err := computePlatformShimFingerprint(flavor, runnerUser)
		if err != nil {
			return "", fmt.Errorf("compute platform shim fingerprint: %w", err)
		}
		payload = struct {
			SchemaVersion string `json:"schema_version"`
			Mode          string `json:"mode"`
			BaseImage     string `json:"base_image"`
			RootfsFlavor  string `json:"rootfs_flavor"`
			RunnerUser    string `json:"runner_user"`
			ThawAgentHash string `json:"thaw_agent_hash"`
			PlatformHash  string `json:"platform_hash"`
			RootfsSizeGB  int    `json:"rootfs_size_gb"`
		}{
			SchemaVersion: "rootfs-source-hash-v2",
			Mode:          "base-image",
			BaseImage:     pinnedBaseImage,
			RootfsFlavor:  string(flavor),
			RunnerUser:    runnerUser,
			ThawAgentHash: thawAgentHash,
			PlatformHash:  platformShimHash,
			RootfsSizeGB:  rootfsSizeGB,
		}
	} else {
		rootfsHash, err := hashFile(rootfsPath)
		if err != nil {
			return "", fmt.Errorf("hash rootfs image: %w", err)
		}
		payload = struct {
			SchemaVersion string `json:"schema_version"`
			Mode          string `json:"mode"`
			RootfsHash    string `json:"rootfs_hash"`
			RootfsSizeGB  int    `json:"rootfs_size_gb"`
		}{
			SchemaVersion: "rootfs-source-hash-v2",
			Mode:          "rootfs-image",
			RootfsHash:    rootfsHash,
			RootfsSizeGB:  rootfsSizeGB,
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal rootfs provenance payload: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func normalizePinnedBaseImage(baseImage string) (string, error) {
	if !strings.Contains(baseImage, "@sha256:") {
		return "", fmt.Errorf("%w: %s", errBaseImageNotPinned, baseImage)
	}
	return baseImage, nil
}

func createExt4Image(path string, sizeGB int, label string) error {
	if sizeGB <= 0 {
		return fmt.Errorf("invalid sizeGB: %d", sizeGB)
	}
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", sizeGB), path).Run(); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}
	// mkfs.ext4 works on regular files with -F
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", label, path).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}
	return nil
}

func getDefaultIface() string {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return "eth0"
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return "eth0"
}

// snapshotSymlinkDir matches the path used by the manager for snapshot symlinks.
// Firecracker opens drive backing files at the paths baked into the snapshot state,
// which are /tmp/snapshot/*.img.
const snapshotSymlinkDir = "/tmp/snapshot"
const hostIP = "172.16.0.1" // Gateway IP for the VM tap network

// restoreFromPreviousSnapshot downloads the previous chunked snapshot from GCS,
// mounts rootfs and extension drives via FUSE, restores the VM from snapshot,
// injects fresh MMDS data with mode=warmup and a new runner_id, and resumes.
// The capsule-thaw-agent detects the runner_id change and re-runs warmup incrementally.
func restoreFromPreviousSnapshot(
	ctx context.Context,
	logger *logrus.Logger,
	log *logrus.Entry,
	vmID, tapName, guestMAC, bootArgs string,
	netnsPath string,
	restoreWorkloadKey string,
	gcpAccessToken string,
	commands []snapshot.SnapshotCommand,
	newDrives []snapshot.DriveSpec,
	runnerID string,
	accessPlaneConfig *accessplane.Config,
) (*firecracker.VM, *fuse.ChunkedDisk, map[string]*fuse.ChunkedDisk, *uffd.Handler, rootfsFlavor, error) {
	// Use a subdirectory for incremental working files to avoid colliding
	// with the symlinks in /tmp/snapshot/ that Firecracker expects.
	incrDir := filepath.Join(*outputDir, "incremental")
	if err := os.MkdirAll(incrDir, 0755); err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create incremental dir: %w", err)
	}

	// 1. Create chunk store and load previous chunked metadata
	chunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
		GCSBucket:      *gcsBucket,
		GCSPrefix:      *gcsPrefix,
		LocalCachePath: filepath.Join(incrDir, "chunk-cache"),
		ChunkSubdir:    "disk",
		Logger:         logger,
	})
	if err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create chunk store: %w", err)
	}

	// Resolve the current version via pointer file, then load its metadata.
	currentVersion, err := chunkStore.ReadCurrentVersion(ctx, restoreWorkloadKey)
	if err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to resolve current version for workload key %q: %w", restoreWorkloadKey, err)
	}
	chunkedMeta, err := chunkStore.LoadChunkedMetadata(ctx, restoreWorkloadKey, currentVersion)
	if err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("no previous chunked snapshot found for version %s: %w", currentVersion, err)
	}
	log.WithFields(logrus.Fields{
		"workload_key":     restoreWorkloadKey,
		"version":          chunkedMeta.Version,
		"rootfs_chunks":    len(chunkedMeta.RootfsChunks),
		"extension_drives": len(chunkedMeta.ExtensionDrives),
	}).Info("Loaded previous chunked snapshot metadata")

	// Check if base rootfs has changed since previous snapshot.
	// If it has (e.g. new capsule-thaw-agent binary, new packages), we must cold boot
	// to pick up the changes — snapshot restore uses the old rootfs.
	if chunkedMeta.RootfsSourceHash != "" {
		provenanceFlavor := rootfsFlavor(chunkedMeta.RootfsFlavor)
		currentHash, err := computeRootfsSourceHash(*rootfsPath, *baseImage, *runnerUser, *thawAgentPath, *rootfsSizeGB, provenanceFlavor)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to compute current rootfs provenance hash: %w", err)
		}
		if currentHash != chunkedMeta.RootfsSourceHash {
			chunkStore.Close()
			log.WithFields(logrus.Fields{
				"previous": chunkedMeta.RootfsSourceHash[:12],
				"current":  currentHash[:12],
			}).Warn("Base rootfs has changed, incremental restore would use stale image")
			return nil, nil, nil, nil, "", fmt.Errorf("rootfs changed (previous=%s current=%s)", chunkedMeta.RootfsSourceHash[:12], currentHash[:12])
		}
		log.WithField("hash", currentHash[:12]).Info("Base rootfs unchanged, safe to restore incrementally")
	} else {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("previous snapshot has no rootfs source hash, cannot verify rootfs unchanged — forcing cold boot")
	}

	// 2. Prepare memory restore: prefer MemFilePath (file-backed), fall back to
	//    UFFD lazy loading from MemChunks.
	localMemPath := filepath.Join(incrDir, "snapshot.mem")
	useUFFD := false
	if chunkedMeta.MemFilePath != "" {
		log.Info("Downloading previous snapshot memory file...")
		if err := chunkStore.DownloadRawFile(ctx, chunkedMeta.MemFilePath, localMemPath); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to download memory file: %w", err)
		}
	} else if len(chunkedMeta.MemChunks) > 0 {
		log.Info("No mem_file_path, will use UFFD lazy loading from memory chunks")
		useUFFD = true
	} else {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("previous snapshot has no mem_file_path and no mem_chunks")
	}

	// 3. Fetch state chunk and write to local file
	localStatePath := filepath.Join(incrDir, "snapshot.state")
	if chunkedMeta.StateHash != "" {
		stateData, err := chunkStore.GetChunk(ctx, chunkedMeta.StateHash)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to fetch vmstate chunk: %w", err)
		}
		if err := os.WriteFile(localStatePath, stateData, 0644); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to write vmstate: %w", err)
		}
		log.WithField("state_size", len(stateData)).Info("Fetched previous vmstate")
	}

	// 4. Fetch kernel chunk and write to local file
	localKernelPath := filepath.Join(incrDir, "kernel.bin")
	if chunkedMeta.KernelHash != "" {
		kernelData, err := chunkStore.GetChunk(ctx, chunkedMeta.KernelHash)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to fetch kernel chunk: %w", err)
		}
		if err := os.WriteFile(localKernelPath, kernelData, 0644); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to write kernel: %w", err)
		}
		log.WithField("kernel_size", len(kernelData)).Info("Fetched kernel from chunk store")
	} else {
		// Fall back to local kernel
		if err := copyFile(*kernelPath, localKernelPath); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to copy kernel: %w", err)
		}
	}

	// 5. Mount rootfs via FUSE (lazy loading with CoW)
	fuseMountDir := filepath.Join(incrDir, "fuse-rootfs")
	fuseDisk, err := fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
		ChunkStore: chunkStore,
		Chunks:     chunkedMeta.RootfsChunks,
		TotalSize:  chunkedMeta.TotalDiskSize,
		ChunkSize:  chunkedMeta.ChunkSize,
		MountPoint: fuseMountDir,
		Logger:     logger,
	})
	if err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create FUSE rootfs disk: %w", err)
	}
	if err := fuseDisk.Mount(); err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to mount FUSE rootfs: %w", err)
	}
	log.Info("Mounted FUSE-backed rootfs from previous snapshot")

	// 6. Mount all extension drives from previous snapshot via FUSE
	fuseExtDisks := make(map[string]*fuse.ChunkedDisk)
	extDrivePaths := make(map[string]string) // driveID → image path
	for driveID, extDrive := range chunkedMeta.ExtensionDrives {
		if len(extDrive.Chunks) == 0 {
			continue
		}
		fuseMountDir := filepath.Join(incrDir, "fuse-ext-"+driveID)
		var totalSize int64
		for _, c := range extDrive.Chunks {
			if end := c.Offset + c.Size; end > totalSize {
				totalSize = end
			}
		}
		extFUSE, fuseErr := fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
			ChunkStore: chunkStore,
			Chunks:     extDrive.Chunks,
			TotalSize:  totalSize,
			ChunkSize:  chunkedMeta.ChunkSize,
			MountPoint: fuseMountDir,
			Logger:     logger,
		})
		if fuseErr != nil {
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to create FUSE ext disk %s: %w", driveID, fuseErr)
		}
		if err := extFUSE.Mount(); err != nil {
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to mount FUSE ext disk %s: %w", driveID, err)
		}
		fuseExtDisks[driveID] = extFUSE
		extDrivePaths[driveID] = extFUSE.DiskImagePath()
		log.WithField("drive_id", driveID).Info("Mounted FUSE-backed extension drive from previous snapshot")
	}

	// 7. Create symlinks in /tmp/snapshot/ so Firecracker can find drives at baked-in paths
	if err := os.MkdirAll(snapshotSymlinkDir, 0755); err != nil {
		fuseDisk.Unmount()
		for _, d := range fuseExtDisks {
			d.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create snapshot symlink dir: %w", err)
	}

	symlinks := []struct {
		name   string
		target string
	}{
		{"rootfs.img", fuseDisk.DiskImagePath()},
	}
	for driveID, path := range extDrivePaths {
		symlinks = append(symlinks, struct{ name, target string }{driveID + ".img", path})
	}
	var createdSymlinks []string
	for _, s := range symlinks {
		linkPath := filepath.Join(snapshotSymlinkDir, s.name)
		os.Remove(linkPath)
		if err := os.Symlink(s.target, linkPath); err != nil {
			for _, c := range createdSymlinks {
				os.Remove(c)
			}
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("symlink %s -> %s: %w", linkPath, s.target, err)
		}
		createdSymlinks = append(createdSymlinks, linkPath)
		log.WithFields(logrus.Fields{
			"link":   linkPath,
			"target": s.target,
		}).Debug("Created snapshot symlink")
	}

	// 9. Create VM and restore from snapshot
	vmCfg := firecracker.VMConfig{
		VMID:           vmID,
		SocketDir:      *outputDir,
		FirecrackerBin: *firecrackerBin,
		KernelPath:     localKernelPath,
		RootfsPath:     fuseDisk.DiskImagePath(),
		VCPUs:          *vcpus,
		MemoryMB:       *memoryMB,
		BootArgs:       bootArgs,
		NetworkIface: &firecracker.NetworkInterface{
			IfaceID:     "eth0",
			HostDevName: tapName,
			GuestMAC:    guestMAC,
		},
		MMDSConfig: &firecracker.MMDSConfig{
			Version:           "V1",
			NetworkInterfaces: []string{"eth0"},
		},
		NetNSPath: netnsPath,
	}

	vm, err := firecracker.NewVM(vmCfg, logger)
	if err != nil {
		for _, c := range createdSymlinks {
			os.Remove(c)
		}
		fuseDisk.Unmount()
		for _, d := range fuseExtDisks {
			d.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create VM: %w", err)
	}

	// Restore VM: use UFFD lazy memory if no MemFilePath, otherwise file-backed.
	var uffdHandler *uffd.Handler
	if useUFFD {
		// Create a dedicated memory chunk store for UFFD.
		memChunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
			GCSBucket:      *gcsBucket,
			GCSPrefix:      *gcsPrefix,
			LocalCachePath: filepath.Join(incrDir, "chunk-cache"),
			ChunkSubdir:    "mem",
			Logger:         logger,
		})
		if err != nil {
			for _, c := range createdSymlinks {
				os.Remove(c)
			}
			vm.Stop()
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to create mem chunk store: %w", err)
		}

		uffdSocketPath := filepath.Join(incrDir, "uffd.sock")
		uffdHandler, err = uffd.NewHandler(uffd.HandlerConfig{
			SocketPath: uffdSocketPath,
			ChunkStore: memChunkStore,
			Metadata:   chunkedMeta,
			Logger:     logger,
		})
		if err != nil {
			memChunkStore.Close()
			for _, c := range createdSymlinks {
				os.Remove(c)
			}
			vm.Stop()
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to create UFFD handler: %w", err)
		}
		if err := uffdHandler.Start(); err != nil {
			memChunkStore.Close()
			for _, c := range createdSymlinks {
				os.Remove(c)
			}
			vm.Stop()
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to start UFFD handler: %w", err)
		}

		log.Info("Restoring VM from previous snapshot with UFFD...")
		if err := vm.RestoreFromSnapshotWithUFFD(ctx, localStatePath, uffdSocketPath, false); err != nil {
			uffdHandler.Stop()
			memChunkStore.Close()
			for _, c := range createdSymlinks {
				os.Remove(c)
			}
			vm.Stop()
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to restore from snapshot with UFFD: %w", err)
		}
	} else {
		log.Info("Restoring VM from previous snapshot (file-backed memory)...")
		if err := vm.RestoreFromSnapshot(ctx, localStatePath, localMemPath, false); err != nil {
			for _, c := range createdSymlinks {
				os.Remove(c)
			}
			vm.Stop()
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to restore from snapshot: %w", err)
		}
	}

	// 10. Clean up symlinks (Firecracker holds fds after LoadSnapshot)
	for _, c := range createdSymlinks {
		os.Remove(c)
	}

	// 11. Set MMDS with mode=warmup and new runner_id
	// The control plane passes the right commands for this build type via --snapshot-commands
	mmdsData := buildWarmupMMDS(commands, newDrives)
	// Override runner_id so capsule-thaw-agent detects the change and re-runs warmup
	mmdsData["latest"].(map[string]interface{})["meta"].(map[string]interface{})["runner_id"] = runnerID
	injectProxyMMDS(mmdsData, accessPlaneConfig)

	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		fuseDisk.Unmount()
		for _, d := range fuseExtDisks {
			d.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to set MMDS data: %w", err)
	}

	// 12. Resume VM — capsule-thaw-agent wakes up, detects runner_id change, re-runs warmup
	log.WithField("runner_id", runnerID).Info("Resuming VM for incremental warmup...")
	if err := vm.Resume(ctx); err != nil {
		vm.Stop()
		fuseDisk.Unmount()
		for _, d := range fuseExtDisks {
			d.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to resume VM: %w", err)
	}

	log.Info("VM restored and resumed for incremental warmup")
	return vm, fuseDisk, fuseExtDisks, uffdHandler, rootfsFlavor(chunkedMeta.RootfsFlavor), nil
}

// reattachFromParent implements the reattach build path.
// It loads the parent layer's VM state (memory, rootfs, vmstate) and the old
// layer's extension drives (which contain valid disk state like cloned repos
// and build caches). The VM is then restored and runs refresh_commands.
func reattachFromParent(
	ctx context.Context,
	logger *logrus.Logger,
	log *logrus.Entry,
	vmID, tapName, guestMAC, bootArgs string,
	netnsPath string,
	gcpAccessToken string,
	commands []snapshot.SnapshotCommand,
	newDrives []snapshot.DriveSpec,
	runnerID string,
	accessPlaneConfig *accessplane.Config,
) (*firecracker.VM, *fuse.ChunkedDisk, map[string]*fuse.ChunkedDisk, *uffd.Handler, rootfsFlavor, error) {
	reattachDir := filepath.Join(*outputDir, "reattach")
	if err := os.MkdirAll(reattachDir, 0755); err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create reattach dir: %w", err)
	}

	// Create chunk store
	chunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
		GCSBucket:      *gcsBucket,
		GCSPrefix:      *gcsPrefix,
		LocalCachePath: filepath.Join(reattachDir, "chunk-cache"),
		ChunkSubdir:    "disk",
		Logger:         logger,
	})
	if err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create chunk store: %w", err)
	}

	// 1. Load parent metadata (for VM state, memory, rootfs)
	parentMeta, err := chunkStore.LoadChunkedMetadata(ctx, *parentWorkloadKey, *parentVersion)
	if err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to load parent metadata: %w", err)
	}
	log.WithFields(logrus.Fields{
		"parent_key":     *parentWorkloadKey,
		"parent_version": *parentVersion,
		"rootfs_chunks":  len(parentMeta.RootfsChunks),
	}).Info("Loaded parent layer metadata for reattach")

	// Check if the current build would produce a different rootfs than the
	// parent snapshot was built from. If so, restoring the old VM state would
	// resume against stale rootfs content and must fall back to cold boot.
	if parentMeta.RootfsSourceHash != "" {
		provenanceFlavor := rootfsFlavor(parentMeta.RootfsFlavor)
		currentHash, err := computeRootfsSourceHash(*rootfsPath, *baseImage, *runnerUser, *thawAgentPath, *rootfsSizeGB, provenanceFlavor)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to compute current rootfs provenance hash: %w", err)
		}
		if currentHash != parentMeta.RootfsSourceHash {
			chunkStore.Close()
			log.WithFields(logrus.Fields{
				"previous": parentMeta.RootfsSourceHash[:12],
				"current":  currentHash[:12],
			}).Warn("Base rootfs has changed, reattach restore would use stale image")
			return nil, nil, nil, nil, "", fmt.Errorf("rootfs changed (previous=%s current=%s)", parentMeta.RootfsSourceHash[:12], currentHash[:12])
		}
		log.WithField("hash", currentHash[:12]).Info("Base rootfs unchanged, safe to restore parent snapshot")
	} else {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("parent snapshot has no rootfs source hash, cannot verify rootfs unchanged — forcing cold boot")
	}

	// 2. Load old layer metadata (for extension drives) — optional for init builds
	var oldLayerMeta *snapshot.ChunkedSnapshotMetadata
	if *previousLayerKey != "" && *previousLayerVersion != "" {
		oldLayerMeta, err = chunkStore.LoadChunkedMetadata(ctx, *previousLayerKey, *previousLayerVersion)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to load old layer metadata: %w", err)
		}
		log.WithFields(logrus.Fields{
			"old_layer_key":     *previousLayerKey,
			"old_layer_version": *previousLayerVersion,
			"extension_drives":  len(oldLayerMeta.ExtensionDrives),
		}).Info("Loaded old layer metadata for reattach (extension drives)")
	} else {
		log.Info("No old layer data (init build from parent), skipping extension drive reuse")
	}

	// 3. Prepare memory restore from parent
	localMemPath := filepath.Join(reattachDir, "snapshot.mem")
	useUFFD := false
	if parentMeta.MemFilePath != "" {
		log.Info("Downloading parent snapshot memory file...")
		if err := chunkStore.DownloadRawFile(ctx, parentMeta.MemFilePath, localMemPath); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to download parent memory: %w", err)
		}
	} else if len(parentMeta.MemChunks) > 0 {
		log.Info("Will use UFFD lazy loading from parent memory chunks")
		useUFFD = true
	} else {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("parent snapshot has no memory data")
	}

	// 4. Fetch parent state chunk
	localStatePath := filepath.Join(reattachDir, "snapshot.state")
	if parentMeta.StateHash != "" {
		stateData, err := chunkStore.GetChunk(ctx, parentMeta.StateHash)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to fetch parent vmstate chunk: %w", err)
		}
		if err := os.WriteFile(localStatePath, stateData, 0644); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to write parent vmstate: %w", err)
		}
	}

	// 5. Fetch kernel chunk from parent
	localKernelPath := filepath.Join(reattachDir, "kernel.bin")
	if parentMeta.KernelHash != "" {
		kernelData, err := chunkStore.GetChunk(ctx, parentMeta.KernelHash)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to fetch parent kernel chunk: %w", err)
		}
		if err := os.WriteFile(localKernelPath, kernelData, 0644); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to write parent kernel: %w", err)
		}
	} else {
		if err := copyFile(*kernelPath, localKernelPath); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to copy kernel: %w", err)
		}
	}

	// 6. Mount parent rootfs via FUSE
	fuseMountDir := filepath.Join(reattachDir, "fuse-rootfs")
	fuseDisk, err := fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
		ChunkStore: chunkStore,
		Chunks:     parentMeta.RootfsChunks,
		TotalSize:  parentMeta.TotalDiskSize,
		ChunkSize:  parentMeta.ChunkSize,
		MountPoint: fuseMountDir,
		Logger:     logger,
	})
	if err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create FUSE rootfs disk: %w", err)
	}
	if err := fuseDisk.Mount(); err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to mount FUSE rootfs: %w", err)
	}
	log.Info("Mounted FUSE-backed rootfs from parent for reattach")

	// 7. Build extension drives for the restored VM.
	// The drives must match what the parent snapshot had.
	fuseExtDisks := make(map[string]*fuse.ChunkedDisk)
	var vmDrives []firecracker.Drive

	// Helper to mount an extension drive from chunked metadata via FUSE
	mountExtDrive := func(driveID, mountSubdir string, meta *snapshot.ChunkedSnapshotMetadata, sourceName string) (string, *fuse.ChunkedDisk, error) {
		ext, ok := meta.ExtensionDrives[driveID]
		if !ok || len(ext.Chunks) == 0 {
			return "", nil, fmt.Errorf("drive %s not found in %s metadata", driveID, sourceName)
		}
		mountDir := filepath.Join(reattachDir, mountSubdir)
		var totalSize int64
		for _, c := range ext.Chunks {
			end := c.Offset + c.Size
			if end > totalSize {
				totalSize = end
			}
		}
		disk, err := fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
			ChunkStore: chunkStore,
			Chunks:     ext.Chunks,
			TotalSize:  totalSize,
			ChunkSize:  meta.ChunkSize,
			MountPoint: mountDir,
			Logger:     logger,
		})
		if err != nil {
			return "", nil, fmt.Errorf("failed to create FUSE %s disk: %w", driveID, err)
		}
		if err := disk.Mount(); err != nil {
			return "", nil, fmt.Errorf("failed to mount FUSE %s disk: %w", driveID, err)
		}
		log.WithFields(logrus.Fields{
			"drive_id": driveID,
			"source":   sourceName,
		}).Info("Mounted FUSE-backed extension drive")
		return disk.DiskImagePath(), disk, nil
	}

	// Mount drives that exist in the parent/old-layer metadata via FUSE.
	// This ensures the kernel's VFS state is consistent with the drive content.
	extSource := oldLayerMeta
	extSourceName := "old layer"
	if extSource == nil {
		extSource = parentMeta
		extSourceName = "parent layer"
	}
	if extSource != nil && len(extSource.ExtensionDrives) > 0 {
		for driveID := range extSource.ExtensionDrives {
			mountSubdir := fmt.Sprintf("fuse-%s", driveID)
			path, disk, err := mountExtDrive(driveID, mountSubdir, extSource, extSourceName)
			if err != nil {
				log.WithError(err).WithField("drive_id", driveID).Warn("Failed to mount extension drive from parent, will create fresh")
				continue
			}
			fuseExtDisks[driveID] = disk
			vmDrives = append(vmDrives, firecracker.Drive{
				DriveID: driveID, PathOnHost: path, IsRootDevice: false, IsReadOnly: false,
			})
		}
	}

	// For layer builds, also create fresh drives for any NEW drives in this
	// layer's config (newDrives) that weren't in the parent snapshot.
	existingDrives := make(map[string]bool)
	for _, d := range vmDrives {
		existingDrives[d.DriveID] = true
	}
	for _, d := range newDrives {
		if existingDrives[d.DriveID] {
			continue // already mounted from parent
		}
		imgPath := filepath.Join(reattachDir, d.DriveID+".img")
		sizeGB := d.SizeGB
		if sizeGB <= 0 {
			sizeGB = 50
		}
		label := d.Label
		if label == "" {
			label = strings.ToUpper(d.DriveID)
		}
		if err := createExt4Image(imgPath, sizeGB, label); err != nil {
			fuseDisk.Unmount()
			chunkStore.Close()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to create drive %s: %w", d.DriveID, err)
		}
		vmDrives = append(vmDrives, firecracker.Drive{
			DriveID: d.DriveID, PathOnHost: imgPath, IsRootDevice: false, IsReadOnly: d.ReadOnly,
		})
		log.WithFields(logrus.Fields{
			"drive_id": d.DriveID,
			"size_gb":  sizeGB,
		}).Info("Created fresh drive for new layer config")
	}

	cleanupFuse := func() {
		for _, d := range fuseExtDisks {
			d.Unmount()
		}
		fuseDisk.Unmount()
		chunkStore.Close()
	}

	// Symlinks for snapshot creation (rootfs is always needed)
	if err := os.MkdirAll(snapshotSymlinkDir, 0755); err != nil {
		cleanupFuse()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create snapshot symlink dir: %w", err)
	}
	symlinkRootfs := filepath.Join(snapshotSymlinkDir, "rootfs.img")
	os.Remove(symlinkRootfs)
	if err := os.Symlink(fuseDisk.DiskImagePath(), symlinkRootfs); err != nil {
		cleanupFuse()
		return nil, nil, nil, nil, "", fmt.Errorf("symlink rootfs: %w", err)
	}
	// Also create symlinks for each drive (used by snapshot upload)
	// Use driveID+".img" (no underscore-to-hyphen) to match cold boot paths
	// that are baked into the snapshot state file.
	var createdSymlinks []string
	createdSymlinks = append(createdSymlinks, symlinkRootfs)
	for _, d := range vmDrives {
		driveName := d.DriveID + ".img"
		linkPath := filepath.Join(snapshotSymlinkDir, driveName)
		os.Remove(linkPath)
		if err := os.Symlink(d.PathOnHost, linkPath); err != nil {
			log.WithError(err).WithField("drive", driveName).Warn("Failed to create drive symlink")
		} else {
			createdSymlinks = append(createdSymlinks, linkPath)
		}
	}

	// 10. Create VM and restore from parent snapshot
	vmCfg := firecracker.VMConfig{
		VMID:           vmID,
		SocketDir:      *outputDir,
		FirecrackerBin: *firecrackerBin,
		KernelPath:     localKernelPath,
		RootfsPath:     fuseDisk.DiskImagePath(),
		VCPUs:          *vcpus,
		MemoryMB:       *memoryMB,
		BootArgs:       bootArgs,
		NetworkIface: &firecracker.NetworkInterface{
			IfaceID:     "eth0",
			HostDevName: tapName,
			GuestMAC:    guestMAC,
		},
		MMDSConfig: &firecracker.MMDSConfig{
			Version:           "V1",
			NetworkInterfaces: []string{"eth0"},
		},
		Drives:    vmDrives,
		NetNSPath: netnsPath,
	}
	vm, err := firecracker.NewVM(vmCfg, logger)
	if err != nil {
		for _, c := range createdSymlinks {
			os.Remove(c)
		}
		cleanupFuse()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to create VM: %w", err)
	}

	cleanupVM := func() {
		for _, c := range createdSymlinks {
			os.Remove(c)
		}
		vm.Stop()
		cleanupFuse()
	}

	var uffdHandler *uffd.Handler
	if useUFFD {
		memChunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
			GCSBucket:      *gcsBucket,
			GCSPrefix:      *gcsPrefix,
			LocalCachePath: filepath.Join(reattachDir, "chunk-cache"),
			ChunkSubdir:    "mem",
			Logger:         logger,
		})
		if err != nil {
			cleanupVM()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to create mem chunk store: %w", err)
		}

		uffdSocketPath := filepath.Join(reattachDir, "uffd.sock")
		uffdHandler, err = uffd.NewHandler(uffd.HandlerConfig{
			SocketPath: uffdSocketPath,
			ChunkStore: memChunkStore,
			Metadata:   parentMeta,
			Logger:     logger,
		})
		if err != nil {
			memChunkStore.Close()
			cleanupVM()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to create UFFD handler: %w", err)
		}
		if err := uffdHandler.Start(); err != nil {
			memChunkStore.Close()
			cleanupVM()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to start UFFD handler: %w", err)
		}

		log.Info("Restoring VM from parent snapshot with UFFD (reattach)...")
		if err := vm.RestoreFromSnapshotWithUFFD(ctx, localStatePath, uffdSocketPath, false); err != nil {
			uffdHandler.Stop()
			memChunkStore.Close()
			cleanupVM()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to restore from parent snapshot with UFFD: %w", err)
		}
	} else {
		log.Info("Restoring VM from parent snapshot (file-backed memory, reattach)...")
		if err := vm.RestoreFromSnapshot(ctx, localStatePath, localMemPath, false); err != nil {
			cleanupVM()
			return nil, nil, nil, nil, "", fmt.Errorf("failed to restore from parent snapshot: %w", err)
		}
	}

	for _, c := range createdSymlinks {
		os.Remove(c)
	}

	mmdsData := buildWarmupMMDS(commands, newDrives)
	// Use the caller-provided runnerID so waitForWarmup's expectedRunnerID matches.
	mmdsData["latest"].(map[string]interface{})["meta"].(map[string]interface{})["runner_id"] = runnerID
	injectProxyMMDS(mmdsData, accessPlaneConfig)

	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		cleanupFuse()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to set MMDS data: %w", err)
	}

	log.WithField("runner_id", runnerID).Info("Resuming VM for reattach warmup...")
	if err := vm.Resume(ctx); err != nil {
		vm.Stop()
		cleanupFuse()
		return nil, nil, nil, nil, "", fmt.Errorf("failed to resume VM: %w", err)
	}

	log.Info("VM restored and resumed for reattach warmup")
	return vm, fuseDisk, fuseExtDisks, uffdHandler, rootfsFlavor(parentMeta.RootfsFlavor), nil
}

// buildWarmupMMDS creates the MMDS data for warmup mode.
// commands are passed through to capsule-thaw-agent as warmup.commands.
func buildWarmupMMDS(commands []snapshot.SnapshotCommand, drives []snapshot.DriveSpec) map[string]interface{} {
	job := map[string]interface{}{}

	return map[string]interface{}{
		"latest": map[string]interface{}{
			"meta": map[string]interface{}{
				"mode":        "warmup",
				"runner_id":   "snapshot-builder",
				"environment": "snapshot-build",
			},
			"warmup": func() map[string]interface{} {
				w := map[string]interface{}{"commands": commands}
				if len(drives) > 0 {
					w["drives"] = drives
				}
				return w
			}(),
			"network": map[string]interface{}{
				"ip":        "172.16.0.2/24",
				"gateway":   "172.16.0.1",
				"netmask":   "255.255.255.0",
				"dns":       "8.8.8.8",
				"interface": "eth0",
			},
			"job": job,
		},
	}
}

// injectProxyMMDS adds access plane proxy metadata to the MMDS data if access plane config is provided.
// The capsule-thaw-agent reads this to configure HTTPS_PROXY, GCE_METADATA_HOST, and install the CA certificate.
func injectProxyMMDS(mmdsData map[string]interface{}, config *accessplane.Config) {
	if config == nil {
		return
	}
	latest := mmdsData["latest"].(map[string]interface{})
	proxyMap := map[string]interface{}{
		"address":      config.ProxyEndpoint,
		"api_endpoint": config.APIEndpoint,
		"tenant_id":    config.TenantID,
	}
	if config.CACertPEM != "" {
		proxyMap["ca_cert_pem"] = config.CACertPEM
	}
	latest["proxy"] = proxyMap
}

// buildRootfsFromImage creates a Firecracker-compatible ext4 rootfs from a Docker image.
// Steps:
//  1. Pull the Docker image
//  2. Create a container and export its filesystem
//  3. Create an ext4 image and populate it from the export
//  4. Inject the platform shim (systemd init, capsule-thaw-agent, networking)
func buildRootfsFromImage(imageURI, outputPath, runnerUser string, log *logrus.Entry) (rootfsFlavor, error) {
	log.WithField("image", imageURI).Info("Pulling Docker image...")
	if output, err := exec.Command("docker", "pull", "--platform=linux/amd64", imageURI).CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker pull failed: %s: %w", string(output), err)
	}

	// Export container filesystem to tar
	log.Info("Exporting container filesystem...")
	containerID, err := exec.Command("docker", "create", "--platform=linux/amd64", imageURI, "/bin/true").Output()
	if err != nil {
		return "", fmt.Errorf("docker create failed: %w", err)
	}
	cid := strings.TrimSpace(string(containerID))
	defer exec.Command("docker", "rm", cid).Run()

	tarPath := outputPath + ".tar"
	tarFile, err := os.Create(tarPath)
	if err != nil {
		return "", fmt.Errorf("failed to create tar file: %w", err)
	}

	exportCmd := exec.Command("docker", "export", cid)
	exportCmd.Stdout = tarFile
	if err := exportCmd.Run(); err != nil {
		tarFile.Close()
		return "", fmt.Errorf("docker export failed: %w", err)
	}
	tarFile.Close()
	defer os.Remove(tarPath)

	// Create ext4 image (8GB default, same as production rootfs)
	rootfsSizeGB := 8
	log.WithField("size_gb", rootfsSizeGB).Info("Creating ext4 rootfs image...")
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", rootfsSizeGB), outputPath).Run(); err != nil {
		return "", fmt.Errorf("truncate failed: %w", err)
	}
	if output, err := exec.Command("mkfs.ext4", "-F", outputPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}

	// Mount and populate
	mountDir := outputPath + ".mnt"
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	if output, err := exec.Command("mount", "-o", "loop", outputPath, mountDir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("mount failed: %s: %w", string(output), err)
	}
	defer exec.Command("umount", mountDir).Run()

	// Extract container filesystem
	log.Info("Extracting container filesystem into rootfs...")
	if output, err := exec.Command("tar", "xf", tarPath, "-C", mountDir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("tar extract failed: %s: %w", string(output), err)
	}

	// Extract Docker image ENV variables before injecting platform shim.
	// Docker ENV directives are image metadata (not on the filesystem), so we
	// need to read them via `docker inspect` and write them into /etc/environment
	// so they're available to all processes inside the Firecracker VM.
	var dockerEnv []string
	inspectOut, err := exec.Command("docker", "inspect", "--format", "{{json .Config.Env}}", imageURI).Output()
	if err != nil {
		log.WithError(err).Warn("Failed to inspect Docker image ENV, skipping ENV injection")
	} else {
		if err := json.Unmarshal(inspectOut, &dockerEnv); err != nil {
			log.WithError(err).Warn("Failed to parse Docker image ENV, skipping ENV injection")
			dockerEnv = nil
		} else {
			log.WithField("env_count", len(dockerEnv)).Info("Extracted Docker image ENV variables")
		}
	}

	// Inject platform shim
	log.Info("Injecting platform shim (systemd, capsule-thaw-agent, networking)...")
	flavor, err := injectPlatformShim(mountDir, runnerUser, *thawAgentPath, dockerEnv, log)
	if err != nil {
		return "", fmt.Errorf("platform shim injection failed: %w", err)
	}

	log.Info("Rootfs built from Docker image successfully")
	return flavor, nil
}

// injectPlatformShim installs the minimal components needed to run a Firecracker
// microVM on top of any Docker image: systemd init, capsule-thaw-agent, network config.
// dockerEnv contains ENV variables extracted from the Docker image config
// (e.g., ["PATH=/opt/venv/bin:/usr/bin", "HOME=/home/user"]) that are written
// to /etc/environment so all processes in the VM inherit them.
func injectPlatformShim(rootfsDir, runnerUser, thawAgentBin string, dockerEnv []string, log *logrus.Entry) (rootfsFlavor, error) {
	cleanupBindMounts := bindRootfsSystemDirs(rootfsDir, log)
	defer cleanupBindMounts()

	flavor, err := detectRootfsFlavor(rootfsDir)
	if err != nil {
		return "", err
	}
	log.WithField("flavor", flavor).Info("Detected rootfs flavor for platform shim")

	if err := seedRootfsResolvConf(rootfsDir); err != nil {
		return "", fmt.Errorf("seed rootfs resolv.conf: %w", err)
	}
	if err := ensurePlatformDependencies(rootfsDir, flavor, log); err != nil {
		return "", err
	}
	if err := installThawAgentBinary(rootfsDir, thawAgentBin); err != nil {
		return "", err
	}
	if err := configureFlavorPlatform(rootfsDir, flavor, runnerUser); err != nil {
		return "", err
	}
	if err := writeCommonRootfsFiles(rootfsDir, dockerEnv, log); err != nil {
		return "", err
	}
	if err := createRunnerUser(rootfsDir, flavor, runnerUser); err != nil {
		return "", err
	}
	if err := ensureWorkspaceOwnership(rootfsDir, runnerUser); err != nil {
		return "", err
	}
	if err := validateInjectedRootfs(rootfsDir, flavor, runnerUser); err != nil {
		return "", fmt.Errorf("platform shim validation failed: %w", err)
	}

	log.WithFields(logrus.Fields{
		"runner_user": runnerUser,
		"flavor":      flavor,
	}).Info("Platform shim injected successfully")
	return flavor, nil
}
