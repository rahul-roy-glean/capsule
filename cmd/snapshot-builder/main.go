package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/authproxy"
	_ "github.com/rahul-roy-glean/bazel-firecracker/pkg/authproxy/providers" // register auth providers
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/firecracker"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/fuse"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/github"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/uffd"
)

var (
	gcsBucket            = flag.String("gcs-bucket", "", "GCS bucket for snapshots")
	outputDir            = flag.String("output-dir", "/tmp/snapshot", "Output directory for snapshot files")
	kernelPath           = flag.String("kernel-path", "/opt/firecracker/kernel.bin", "Path to kernel")
	rootfsPath           = flag.String("rootfs-path", "/opt/firecracker/rootfs.img", "Path to base rootfs")
	firecrackerBin       = flag.String("firecracker-bin", "/usr/local/bin/firecracker", "Path to firecracker binary")
	vcpus                = flag.Int("vcpus", 4, "vCPUs for warmup VM")
	memoryMB             = flag.Int("memory-mb", 8192, "Memory MB for warmup VM")
	warmupTimeout        = flag.Duration("warmup-timeout", 30*time.Minute, "Timeout for warmup phase")
	rootfsSizeGB         = flag.Int("rootfs-size-gb", 0, "Expand rootfs to this size in GB (0 = keep original size). Increase if bazel fetch runs out of space.")
	logLevel             = flag.String("log-level", "info", "Log level")
	enableChunked        = flag.Bool("enable-chunked", true, "Also build a chunked snapshot for lazy loading")
	memBackend           = flag.String("mem-backend", "chunked", "Memory backend for chunked snapshots: 'chunked' (UFFD lazy loading via MemChunks, default) or 'file' (upload snapshot.mem as single blob)")
	gcsPrefix            = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")

	versionOverride = flag.String("version", "", "Snapshot version string (if empty, auto-generated from timestamp + workload key)")

	// Snapshot commands (replaces --repo-slug/--repo-url/--repo-branch/--bazel-version)
	snapshotCommands = flag.String("snapshot-commands", "", "JSON array of SnapshotCommand describing what to bake into the snapshot (required)")

	// GitHub App authentication for private repos (used when git-cache is not available)
	githubAppID     = flag.String("github-app-id", "", "GitHub App ID for private repo access")
	githubAppSecret = flag.String("github-app-secret", "", "GCP Secret Manager secret name containing GitHub App private key")
	gcpProject      = flag.String("gcp-project", "", "GCP project for Secret Manager (defaults to metadata project)")

	// Layer build flags
	layerHash            = flag.String("layer-hash", "", "Layer hash for metadata tagging")
	parentWorkloadKey    = flag.String("parent-workload-key", "", "Parent layer's hash, used as GCS workload key to load parent snapshot")
	parentVersion        = flag.String("parent-version", "", "Version of parent snapshot to restore from")
	layerDrives          = flag.String("layer-drives", "", "JSON array of DriveSpec for this layer's new extension drives")
	buildType            = flag.String("build-type", "init", "Build type: init, refresh, or reattach")
	previousLayerKey     = flag.String("previous-layer-key", "", "Old layer hash for loading extension drives during reattach")
	previousLayerVersion = flag.String("previous-layer-version", "", "Version of old layer to load drives from")

	// Base image support: build rootfs from Docker image instead of pre-baked rootfs.img
	baseImage  = flag.String("base-image", "", "Docker image URI to use as rootfs base (e.g. 'ubuntu:22.04'). When set, pulls the image, converts to ext4, and installs thaw-agent.")
	runnerUser = flag.String("runner-user", "runner", "Username for non-root commands inside the VM")

	// Auth proxy: transparent credential injection for the warmup VM
	authConfigJSON = flag.String("auth-config", "", "JSON auth proxy config (AuthConfig). When set, starts an auth proxy on the host to provide GCP metadata and HTTPS credential injection.")
)

func main() {
	flag.Parse()

	// Setup logger
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	log := logger.WithField("component", "snapshot-builder")
	log.Info("Starting snapshot builder")

	if *snapshotCommands == "" {
		log.Fatal("--snapshot-commands is required")
	}
	if *gcsBucket == "" {
		log.Fatal("--gcs-bucket is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *warmupTimeout+30*time.Minute)
	defer cancel()

	// Generate GitHub token for private repo access (if GitHub App is configured)
	var gitToken string
	if *githubAppID != "" && *githubAppSecret != "" {
		project := *gcpProject
		if project == "" {
			project = os.Getenv("GCP_PROJECT")
		}
		if project == "" {
			log.Fatal("--gcp-project is required when using GitHub App authentication")
		}

		log.WithFields(logrus.Fields{
			"app_id":      *githubAppID,
			"secret":      *githubAppSecret,
			"gcp_project": project,
		}).Info("Using GitHub App for private repo authentication")

		tokenClient, err := github.NewTokenClient(ctx, *githubAppID, *githubAppSecret, project)
		if err != nil {
			log.WithError(err).Fatal("Failed to create GitHub App token client")
		}

		installToken, err := tokenClient.GetInstallationToken(ctx)
		if err != nil {
			log.WithError(err).Fatal("Failed to get GitHub App installation token")
		}

		if installToken == "" {
			log.Fatal("GitHub App installation token is empty - check App permissions and installation")
		}
		gitToken = installToken
		log.Info("Successfully obtained GitHub App installation token for warmup")
	}

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
		log.WithError(err).Fatal("invalid --snapshot-commands")
	}
	if len(commands) == 0 {
		log.Fatal("--snapshot-commands must be non-empty")
	}
	workloadKey := snapshot.ComputeWorkloadKey(commands)
	log.WithField("workload_key", workloadKey).Info("Computed workload key from snapshot commands")

	// Layer mode: use layer_hash as GCS directory key
	effectiveWorkloadKey := workloadKey
	if *layerHash != "" {
		effectiveWorkloadKey = *layerHash
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

	// Generate version string (use override from control plane if provided)
	version := *versionOverride
	if version == "" {
		version = fmt.Sprintf("v%s-%s", time.Now().Format("20060102-150405"), effectiveWorkloadKey[:16])
	}
	log.WithField("version", version).Info("Building snapshot")

	// Create output directory
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.WithError(err).Fatal("Failed to create output directory")
	}

	// Network constants shared by both cold boot and incremental paths
	vmID := "snapshot-builder"
	tapName := "tap-slot-0"         // Must match manager's slot naming for snapshot compatibility
	guestIP := "172.16.0.2"         // Slot 0 always gets .2
	guestMAC := "AA:FC:00:00:00:02" // Deterministic MAC based on slot
	// hostIP is a package-level constant (172.16.0.1)
	netmask := "255.255.255.0"

	log.Info("Setting up network for warmup VM...")
	if err := setupWarmupNetwork(tapName, hostIP); err != nil {
		log.WithError(err).Fatal("Failed to setup warmup network")
	}
	defer cleanupWarmupNetwork(tapName)

	// Start auth proxy if configured. The proxy provides:
	// 1. GCP metadata server emulation on hostIP:80 (for keyrings.google-artifactregistry-auth)
	// 2. HTTPS MITM proxy on hostIP:3128 (injects auth headers for matched hosts)
	var authProxy *authproxy.AuthProxy
	var authProxyAddr string // e.g. "http://172.16.0.1:3128"
	if *authConfigJSON != "" {
		var authCfg authproxy.AuthConfig
		if err := json.Unmarshal([]byte(*authConfigJSON), &authCfg); err != nil {
			log.WithError(err).Fatal("Failed to parse --auth-config JSON")
		}
		proxyPort := authCfg.Proxy.ListenPort
		if proxyPort == 0 {
			proxyPort = 3128
		}
		authProxyAddr = fmt.Sprintf("http://%s:%d", hostIP, proxyPort)
		var proxyErr error
		authProxy, proxyErr = authproxy.NewAuthProxy("snapshot-builder", authCfg, "", hostIP, "", log)
		if proxyErr != nil {
			log.WithError(proxyErr).Fatal("Failed to create auth proxy")
		}
		if proxyErr = authProxy.Start(ctx); proxyErr != nil {
			log.WithError(proxyErr).Fatal("Failed to start auth proxy")
		}
		defer authProxy.Stop()
		// Note: DNAT to 169.254.169.254 doesn't work because Firecracker's MMDS
		// intercepts all traffic to that IP before it reaches the tap interface.
		// Instead, we pass the metadata host (gatewayIP) via MMDS and set
		// GCE_METADATA_HOST in the guest so google-auth talks directly to the proxy.
	}

	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init ip=%s::%s:%s::eth0:off",
		guestIP, hostIP, netmask)

	// Track FUSE disks for cleanup and incremental chunking
	var vm *firecracker.VM
	var fuseDisk *fuse.ChunkedDisk
	var fuseExtDisks map[string]*fuse.ChunkedDisk // driveID → FUSE disk for extension drives
	var incrUffdHandler *uffd.Handler

	// Paths used by both paths and for final snapshot creation
	workingRootfs := filepath.Join(*outputDir, "rootfs.img")

	// rootfsSourceHash is lazily computed and stored in chunked metadata
	// so future incremental builds can detect rootfs changes.
	var rootfsSourceHash string

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
			vm, fuseDisk, fuseExtDisks, incrUffdHandler, reattachErr = reattachFromParent(ctx, logger, log, vmID, tapName, guestMAC, bootArgs, gitToken, gcpAccessToken, commands, newDrives, expectedRunnerID, authProxy, authProxyAddr)
			if reattachErr != nil {
				log.WithError(reattachErr).Warn("Reattach failed, falling back to cold boot")
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
		} else {
			log.Info("Attempting incremental restore from previous snapshot...")
			var incrementalErr error
			vm, fuseDisk, fuseExtDisks, incrUffdHandler, incrementalErr = restoreFromPreviousSnapshot(ctx, logger, log, vmID, tapName, guestMAC, bootArgs, gitToken, gcpAccessToken, commands, newDrives, expectedRunnerID, authProxy, authProxyAddr)
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
			if err := buildRootfsFromImage(*baseImage, workingRootfs, *runnerUser, log); err != nil {
				log.WithError(err).Fatal("Failed to build rootfs from Docker image")
			}
		} else {
			// Legacy path: copy pre-baked rootfs.img
			log.Info("Creating working rootfs from pre-baked image...")
			if err := copyFile(*rootfsPath, workingRootfs); err != nil {
				log.WithError(err).Fatal("Failed to copy rootfs")
			}
		}
		if *rootfsSizeGB > 0 {
			log.WithField("size_gb", *rootfsSizeGB).Info("Expanding rootfs...")
			if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", *rootfsSizeGB), workingRootfs).Run(); err != nil {
				log.WithError(err).Fatal("Failed to expand rootfs")
			}
			if output, err := exec.Command("e2fsck", "-fy", workingRootfs).CombinedOutput(); err != nil {
				log.WithField("output", string(output)).Warn("e2fsck returned non-zero (may be OK)")
			}
			if output, err := exec.Command("resize2fs", workingRootfs).CombinedOutput(); err != nil {
				log.WithField("output", string(output)).Fatal("Failed to resize2fs rootfs")
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
					log.WithError(err).WithField("drive_id", d.DriveID).Fatal("Failed to create drive image")
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
			NetworkIface: &firecracker.NetworkInterface{
				IfaceID:     "eth0",
				HostDevName: tapName,
				GuestMAC:    guestMAC,
			},
			MMDSConfig: &firecracker.MMDSConfig{
				Version:           "V1",
				NetworkInterfaces: []string{"eth0"},
			},
			Drives: drives,
		}

		var err error
		vm, err = firecracker.NewVM(vmCfg, logger)
		if err != nil {
			log.WithError(err).Fatal("Failed to create VM")
		}

		log.Info("Starting warmup VM...")
		if err := vm.Start(ctx); err != nil {
			log.WithError(err).Fatal("Failed to start VM")
		}

		mmdsData := buildWarmupMMDS(commands, gitToken, newDrives)
		injectProxyMMDS(mmdsData, authProxy, authProxyAddr, hostIP)
		if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
			vm.Stop()
			log.WithError(err).Fatal("Failed to set MMDS data")
		}
	}

	// Shared path: wait for warmup, snapshot, upload
	log.Info("Waiting for warmup to complete...")
	warmupCtx, warmupCancel := context.WithTimeout(ctx, *warmupTimeout)
	defer warmupCancel()

	if err := waitForWarmup(warmupCtx, vm, guestIP, expectedRunnerID, log); err != nil {
		vm.Stop()
		log.WithError(err).Fatal("Warmup failed")
	}

	// Create snapshot
	log.Info("Creating snapshot...")
	snapshotPath := filepath.Join(*outputDir, "snapshot.state")
	memPath := filepath.Join(*outputDir, "snapshot.mem")

	if err := vm.CreateSnapshot(ctx, snapshotPath, memPath); err != nil {
		vm.Stop()
		log.WithError(err).Fatal("Failed to create snapshot")
	}

	// Stop VM
	vm.Stop()

	// Stop UFFD handler (if used for incremental restore)
	if incrUffdHandler != nil {
		incrUffdHandler.Stop()
		incrUffdHandler = nil
	}

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
			log.WithError(err).Fatal("Failed to save dirty rootfs chunks")
		}
		log.WithFields(logrus.Fields{
			"total_chunks": len(incrementalRootfsChunks),
			"dirty_chunks": fuseDisk.DirtyChunkCount(),
		}).Info("Rootfs dirty chunks saved to chunk store")
		fuseDisk.Unmount()
		fuseDisk = nil
	}
	for driveID, extDisk := range fuseExtDisks {
		log.WithFields(logrus.Fields{
			"drive_id":     driveID,
			"dirty_chunks": extDisk.DirtyChunkCount(),
		}).Info("Saving FUSE extension drive dirty chunks to chunk store...")
		chunks, err := extDisk.SaveDirtyChunks(ctx)
		if err != nil {
			log.WithError(err).WithField("drive_id", driveID).Fatal("Failed to save dirty extension drive chunks")
		}
		incrementalExtChunks[driveID] = chunks
		extDisk.Unmount()
	}

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
			log.Fatal("Failed to find kernel in incremental or reattach dir")
		}
		if err := copyFile(kernelSrc, kernelOutput); err != nil {
			log.WithError(err).Fatal("Failed to copy kernel from restore dir")
		}
	} else {
		// Cold boot: copy kernel from flag path
		if err := copyFile(*kernelPath, kernelOutput); err != nil {
			log.WithError(err).Fatal("Failed to copy kernel")
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

	// Derive informational repo URL from git-clone command (best-effort, for metadata only).
	metaRepoURL := ""
	for _, cmd := range commands {
		if cmd.Type == "git-clone" && len(cmd.Args) > 0 {
			metaRepoURL = cmd.Args[0]
			break
		}
	}

	// Create metadata
	metadata := snapshot.SnapshotMetadata{
		Version:           version,
		RepoCommit:        getGitCommit(*outputDir),
		Repo:              metaRepoURL,
		WorkloadKey:       workloadKey,
		Commands:          commands,
		CreatedAt:         time.Now(),
		SizeBytes:         totalSize,
		KernelPath:        "kernel.bin",
		RootfsPath:        "rootfs.img",
		MemPath:           "snapshot.mem",
		StatePath:         "snapshot.state",
	}

	// Upload to GCS
	uploader, err := snapshot.NewUploader(ctx, snapshot.UploaderConfig{
		GCSBucket: *gcsBucket,
		GCSPrefix: *gcsPrefix,
		Logger:    logger,
	})
	if err != nil {
		log.WithError(err).Fatal("Failed to create uploader")
	}
	defer uploader.Close()

	if *enableChunked {
		log.Info("Chunked mode: skipping full-file upload")
	} else if !wasIncremental {
		// Legacy upload only works with cold boot (needs full files on disk)
		log.Info("Uploading full snapshot to GCS...")
		if err := uploader.UploadSnapshot(ctx, *outputDir, metadata); err != nil {
			log.WithError(err).Fatal("Failed to upload snapshot")
		}
	}

	// Build and upload chunked snapshot for lazy loading
	if *enableChunked {
		log.Info("Building chunked snapshot for lazy loading...")

		chunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
			GCSBucket:      *gcsBucket,
			GCSPrefix:      *gcsPrefix,
			LocalCachePath: filepath.Join(*outputDir, "chunk-cache"),
			ChunkSubdir:    "disk",
			Logger:         logger,
		})
		if err != nil {
			log.WithError(err).Fatal("Failed to create chunk store")
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
			log.WithError(err).Fatal("Failed to create mem chunk store")
		}
		defer memChunkStore.Close()

		builder := snapshot.NewChunkedSnapshotBuilder(chunkStore, memChunkStore, logger)
		builder.MemBackend = *memBackend

		// Compute rootfs source hash for storing in metadata (enables incremental change detection).
		// Only needed at upload time — the incremental path hashes independently for comparison.
		if rootfsSourceHash == "" {
			if h, err := hashFile(*rootfsPath); err == nil {
				rootfsSourceHash = h
				log.WithField("hash", rootfsSourceHash[:12]).Info("Base rootfs hash computed for metadata")
			} else {
				log.WithError(err).Warn("Failed to hash base rootfs, future incremental builds won't detect rootfs changes")
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
				log.WithError(err).Fatal("Failed to read kernel for chunking")
			}
			kernelHash, _, err := chunkStore.StoreChunk(ctx, kernelData)
			if err != nil {
				log.WithError(err).Fatal("Failed to store kernel chunk")
			}
			chunkedMeta.KernelHash = kernelHash

			// Chunk state
			stateData, err := os.ReadFile(snapshotPath)
			if err != nil {
				log.WithError(err).Fatal("Failed to read state for chunking")
			}
			stateHash, _, err := chunkStore.StoreChunk(ctx, stateData)
			if err != nil {
				log.WithError(err).Fatal("Failed to store state chunk")
			}
			chunkedMeta.StateHash = stateHash

			// Store memory according to --mem-backend flag.
			if *memBackend == "file" {
				memGCSPath := fmt.Sprintf("%s/snapshot_state/%s/snapshot.mem.zst", effectiveWorkloadKey, version)
				_, _, err = memChunkStore.UploadRawFile(ctx, memPath, memGCSPath)
				if err != nil {
					log.WithError(err).Fatal("Failed to upload raw memory file")
				}
				chunkedMeta.MemFilePath = memGCSPath
			} else {
				memChunks, err := memChunkStore.ChunkFile(ctx, memPath, snapshot.DefaultChunkSize)
				if err != nil {
					log.WithError(err).Fatal("Failed to chunk memory file")
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
				for driveID, chunks := range incrementalExtChunks {
					var totalSize int64
					for _, c := range chunks {
						if end := c.Offset + c.Size; end > totalSize {
							totalSize = end
						}
					}
					chunkedMeta.ExtensionDrives[driveID] = snapshot.ExtensionDrive{
						Chunks:    chunks,
						ReadOnly:  false,
						SizeBytes: totalSize,
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
					log.WithError(chunkErr).WithField("drive_id", d.DriveID).Fatal("Failed to chunk extension drive")
				}
				if chunkedMeta.ExtensionDrives == nil {
					chunkedMeta.ExtensionDrives = make(map[string]snapshot.ExtensionDrive)
				}
				stat, _ := os.Stat(imgPath)
				chunkedMeta.ExtensionDrives[d.DriveID] = snapshot.ExtensionDrive{
					Chunks:    chunks,
					ReadOnly:  d.ReadOnly,
					SizeBytes: stat.Size(),
				}
			}

			if err := builder.UploadChunkedMetadata(ctx, chunkedMeta); err != nil {
				log.WithError(err).Fatal("Failed to upload chunked metadata")
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
				log.WithError(err).Fatal("Failed to build chunked snapshot")
			}
			chunkedMeta.RootfsSourceHash = rootfsSourceHash
			chunkedMeta.Commands = commands
			// Populate layer fields if in layer mode
			if *layerHash != "" {
				chunkedMeta.LayerHash = *layerHash
				chunkedMeta.ParentLayerHash = *parentWorkloadKey
				chunkedMeta.ParentVersion = *parentVersion
			}

			if err := builder.UploadChunkedMetadata(ctx, chunkedMeta); err != nil {
				log.WithError(err).Fatal("Failed to upload chunked metadata")
			}

			log.WithFields(logrus.Fields{
				"mem_chunks":       len(chunkedMeta.MemChunks),
				"disk_chunks":      len(chunkedMeta.RootfsChunks),
				"extension_drives": len(chunkedMeta.ExtensionDrives),
			}).Info("Chunked snapshot built and uploaded")
		}
	}

	// Update current pointer (workload-key-scoped) — done last so the pointer
	// only moves after all snapshot data has been fully uploaded.
	log.Info("Updating current pointer...")
	if err := uploader.UpdateCurrentPointerForRepo(ctx, version, effectiveWorkloadKey); err != nil {
		log.WithError(err).Fatal("Failed to update current pointer")
	}

	log.WithFields(logrus.Fields{
		"version":    version,
		"size_bytes": totalSize,
		"gcs_path":   fmt.Sprintf("gs://%s/%s/", *gcsBucket, version),
	}).Info("Snapshot build complete!")

	// Cleanup
	socketPath := filepath.Join(*outputDir, vmID+".sock")
	os.Remove(socketPath)
}

func waitForWarmup(ctx context.Context, vm *firecracker.VM, guestIP string, expectedRunnerID string, log *logrus.Entry) error {
	// Wait for thaw-agent health endpoint to become available
	// The thaw-agent runs warmup and exposes /health and /warmup-status endpoints

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

	// Phase 1: Wait for VM to boot and thaw-agent to start
	log.Info("Phase 1: Waiting for thaw-agent to become responsive...")
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
			// The thaw-agent tags warmup status with the runner_id so we can
			// detect when it hasn't yet picked up the new MMDS data.
			if expectedRunnerID != "" && status.RunnerID != expectedRunnerID {
				log.WithFields(logrus.Fields{
					"status_runner_id":   status.RunnerID,
					"expected_runner_id": expectedRunnerID,
				}).Debug("Ignoring stale warmup status from previous layer")
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
			log.Debug("Waiting for thaw-agent...")
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

// WarmupStatus represents the warmup status from thaw-agent
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

func createExt4ImageMB(path string, sizeMB int, label string) error {
	if sizeMB <= 0 {
		return fmt.Errorf("invalid sizeMB: %d", sizeMB)
	}
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dM", sizeMB), path).Run(); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}
	// mkfs.ext4 works on regular files with -F
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", label, path).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}
	return nil
}

func seedExt4ImageFromDir(imgPath, seedDir string, log *logrus.Entry) error {
	info, err := os.Stat(seedDir)
	if err != nil {
		return fmt.Errorf("seed dir stat failed: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("seed dir is not a directory: %s", seedDir)
	}

	mountPoint := filepath.Join(filepath.Dir(imgPath), "mnt-repo-cache-seed")
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}
	defer os.RemoveAll(mountPoint)

	// Mount loopback image (requires root)
	if output, err := exec.Command("mount", "-o", "loop", imgPath, mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("mount loop failed: %s: %w", string(output), err)
	}
	defer func() {
		if output, err := exec.Command("umount", mountPoint).CombinedOutput(); err != nil {
			log.WithError(err).WithField("output", string(output)).Warn("Failed to unmount repo-cache seed image")
		}
	}()

	// Copy seed content into the image root
	// We prefer rsync for correctness (preserve permissions, symlinks) if available.
	if _, err := exec.LookPath("rsync"); err == nil {
		cmd := exec.Command("rsync", "-a", "--delete", seedDir+string(os.PathSeparator), mountPoint+string(os.PathSeparator))
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("rsync failed: %s: %w", string(output), err)
		}
		return nil
	}

	cmd := exec.Command("cp", "-a", seedDir+string(os.PathSeparator)+".", mountPoint+string(os.PathSeparator))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp -a failed: %s: %w", string(output), err)
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

func getGitCommit(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	commit := strings.TrimSpace(string(out))
	if len(commit) > 40 {
		return commit[:40]
	}
	return commit
}

// setupWarmupNetwork creates the TAP device and connects it to the existing bridge.
// The host is expected to have a bridge (fcbr0) with IP 172.16.0.1/24 already configured.
func setupWarmupNetwork(tapName, hostIP string) error {
	// Create TAP device
	if output, err := exec.Command("ip", "tuntap", "add", "dev", tapName, "mode", "tap").CombinedOutput(); err != nil {
		// Might already exist, try to continue
		if !strings.Contains(string(output), "exists") {
			return fmt.Errorf("failed to create tap: %s: %w", string(output), err)
		}
	}

	// Try to connect TAP to existing bridge (fcbr0)
	// This is the preferred setup when the host already has a bridge configured
	bridgeName := "fcbr0"
	if output, err := exec.Command("ip", "link", "set", tapName, "master", bridgeName).CombinedOutput(); err != nil {
		// If bridge doesn't exist, fall back to standalone TAP with its own IP
		fmt.Printf("Note: Could not add TAP to bridge %s (%s), using standalone network\n", bridgeName, strings.TrimSpace(string(output)))
		// Configure TAP IP directly
		if output, err := exec.Command("ip", "addr", "add", hostIP+"/24", "dev", tapName).CombinedOutput(); err != nil {
			if !strings.Contains(string(output), "exists") {
				return fmt.Errorf("failed to add ip: %s: %w", string(output), err)
			}
		}
	}

	// Match TAP/bridge MTU to the host's outbound interface (GCP uses 1460, not 1500)
	hostMTU := "1460" // safe default for GCP
	if mtuBytes, err := os.ReadFile("/sys/class/net/" + getDefaultIface() + "/mtu"); err == nil {
		hostMTU = strings.TrimSpace(string(mtuBytes))
	}
	exec.Command("ip", "link", "set", tapName, "mtu", hostMTU).Run()
	if bridgeName == "fcbr0" {
		exec.Command("ip", "link", "set", bridgeName, "mtu", hostMTU).Run()
	}

	// Bring TAP up
	if output, err := exec.Command("ip", "link", "set", tapName, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("failed to bring tap up: %s: %w", string(output), err)
	}

	// Enable IP forwarding
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)

	// Setup NAT for outbound traffic (guest needs internet for warmup)
	// These rules may already exist from bridge setup, but adding them again is harmless
	exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "172.16.0.0/24", "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-A", "FORWARD", "-i", tapName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-A", "FORWARD", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()

	// Clamp TCP MSS to path MTU. The guest may have MTU 1500 (default) while the host
	// outbound interface uses 1460 (GCP). Without this, large TCP segments from the guest
	// get dropped after NAT because they exceed the host MTU and DF is set.
	exec.Command("iptables", "-t", "mangle", "-A", "FORWARD", "-p", "tcp",
		"--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu").Run()

	return nil
}

// cleanupWarmupNetwork removes the TAP device and iptables rules
func cleanupWarmupNetwork(tapName string) {
	exec.Command("ip", "link", "set", tapName, "down").Run()
	exec.Command("ip", "tuntap", "del", "dev", tapName, "mode", "tap").Run()
	exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", "172.16.0.0/24", "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-D", "FORWARD", "-i", tapName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-D", "FORWARD", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
}

// snapshotSymlinkDir matches the path used by the manager for snapshot symlinks.
// Firecracker opens drive backing files at the paths baked into the snapshot state,
// which are /tmp/snapshot/*.img.
const snapshotSymlinkDir = "/tmp/snapshot"
const hostIP = "172.16.0.1" // Gateway IP for the VM tap network

// restoreFromPreviousSnapshot downloads the previous chunked snapshot from GCS,
// mounts rootfs and repo-cache-seed via FUSE, restores the VM from snapshot,
// injects fresh MMDS data with mode=warmup and a new runner_id, and resumes.
// The thaw-agent detects the runner_id change and re-runs warmup incrementally.
func restoreFromPreviousSnapshot(
	ctx context.Context,
	logger *logrus.Logger,
	log *logrus.Entry,
	vmID, tapName, guestMAC, bootArgs string,
	gitToken, gcpAccessToken string,
	commands []snapshot.SnapshotCommand,
	newDrives []snapshot.DriveSpec,
	runnerID string,
	authProxy *authproxy.AuthProxy, authProxyAddr string,
) (*firecracker.VM, *fuse.ChunkedDisk, map[string]*fuse.ChunkedDisk, *uffd.Handler, error) {
	// Use a subdirectory for incremental working files to avoid colliding
	// with the symlinks in /tmp/snapshot/ that Firecracker expects.
	incrDir := filepath.Join(*outputDir, "incremental")
	if err := os.MkdirAll(incrDir, 0755); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to create incremental dir: %w", err)
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
		return nil, nil, nil, nil, fmt.Errorf("failed to create chunk store: %w", err)
	}

	// Resolve the current version via pointer file, then load its metadata.
	wk := snapshot.ComputeWorkloadKey(commands)
	currentVersion, err := chunkStore.ReadCurrentVersion(ctx, wk)
	if err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to resolve current version for workload key %q: %w", wk, err)
	}
	chunkedMeta, err := chunkStore.LoadChunkedMetadata(ctx, wk, currentVersion)
	if err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("no previous chunked snapshot found for version %s: %w", currentVersion, err)
	}
	log.WithFields(logrus.Fields{
		"version":          chunkedMeta.Version,
		"rootfs_chunks":    len(chunkedMeta.RootfsChunks),
		"extension_drives": len(chunkedMeta.ExtensionDrives),
	}).Info("Loaded previous chunked snapshot metadata")

	// Check if base rootfs has changed since previous snapshot.
	// If it has (e.g. new thaw-agent binary, new packages), we must cold boot
	// to pick up the changes — snapshot restore uses the old rootfs.
	if chunkedMeta.RootfsSourceHash != "" {
		currentHash, err := hashFile(*rootfsPath)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to hash current rootfs: %w", err)
		}
		if currentHash != chunkedMeta.RootfsSourceHash {
			chunkStore.Close()
			log.WithFields(logrus.Fields{
				"previous": chunkedMeta.RootfsSourceHash[:12],
				"current":  currentHash[:12],
			}).Warn("Base rootfs has changed, incremental restore would use stale image")
			return nil, nil, nil, nil, fmt.Errorf("rootfs changed (previous=%s current=%s)", chunkedMeta.RootfsSourceHash[:12], currentHash[:12])
		}
		log.WithField("hash", currentHash[:12]).Info("Base rootfs unchanged, safe to restore incrementally")
	} else {
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("previous snapshot has no rootfs source hash, cannot verify rootfs unchanged — forcing cold boot")
	}

	// 2. Prepare memory restore: prefer MemFilePath (file-backed), fall back to
	//    UFFD lazy loading from MemChunks.
	localMemPath := filepath.Join(incrDir, "snapshot.mem")
	useUFFD := false
	if chunkedMeta.MemFilePath != "" {
		log.Info("Downloading previous snapshot memory file...")
		if err := chunkStore.DownloadRawFile(ctx, chunkedMeta.MemFilePath, localMemPath); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to download memory file: %w", err)
		}
	} else if len(chunkedMeta.MemChunks) > 0 {
		log.Info("No mem_file_path, will use UFFD lazy loading from memory chunks")
		useUFFD = true
	} else {
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("previous snapshot has no mem_file_path and no mem_chunks")
	}

	// 3. Fetch state chunk and write to local file
	localStatePath := filepath.Join(incrDir, "snapshot.state")
	if chunkedMeta.StateHash != "" {
		stateData, err := chunkStore.GetChunk(ctx, chunkedMeta.StateHash)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to fetch vmstate chunk: %w", err)
		}
		if err := os.WriteFile(localStatePath, stateData, 0644); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to write vmstate: %w", err)
		}
		log.WithField("state_size", len(stateData)).Info("Fetched previous vmstate")
	}

	// 4. Fetch kernel chunk and write to local file
	localKernelPath := filepath.Join(incrDir, "kernel.bin")
	if chunkedMeta.KernelHash != "" {
		kernelData, err := chunkStore.GetChunk(ctx, chunkedMeta.KernelHash)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to fetch kernel chunk: %w", err)
		}
		if err := os.WriteFile(localKernelPath, kernelData, 0644); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to write kernel: %w", err)
		}
		log.WithField("kernel_size", len(kernelData)).Info("Fetched kernel from chunk store")
	} else {
		// Fall back to local kernel
		if err := copyFile(*kernelPath, localKernelPath); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to copy kernel: %w", err)
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
		return nil, nil, nil, nil, fmt.Errorf("failed to create FUSE rootfs disk: %w", err)
	}
	if err := fuseDisk.Mount(); err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to mount FUSE rootfs: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to create FUSE ext disk %s: %w", driveID, fuseErr)
		}
		if err := extFUSE.Mount(); err != nil {
			fuseDisk.Unmount()
			for _, d := range fuseExtDisks {
				d.Unmount()
			}
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to mount FUSE ext disk %s: %w", driveID, err)
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
		return nil, nil, nil, nil, fmt.Errorf("failed to create snapshot symlink dir: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("symlink %s -> %s: %w", linkPath, s.target, err)
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
		return nil, nil, nil, nil, fmt.Errorf("failed to create VM: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to create mem chunk store: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to create UFFD handler: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to start UFFD handler: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to restore from snapshot with UFFD: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to restore from snapshot: %w", err)
		}
	}

	// 10. Clean up symlinks (Firecracker holds fds after LoadSnapshot)
	for _, c := range createdSymlinks {
		os.Remove(c)
	}

	// 11. Set MMDS with mode=warmup and new runner_id
	newRunnerID := fmt.Sprintf("snapshot-builder-incr-%s", uuid.New().String()[:8])
	// The control plane passes the right commands for this build type via --snapshot-commands
	mmdsData := buildWarmupMMDS(commands, gitToken, newDrives)
	// Override runner_id so thaw-agent detects the change and re-runs warmup
	mmdsData["latest"].(map[string]interface{})["meta"].(map[string]interface{})["runner_id"] = runnerID
	injectProxyMMDS(mmdsData, authProxy, authProxyAddr, hostIP)

	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		fuseDisk.Unmount()
		for _, d := range fuseExtDisks {
			d.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to set MMDS data: %w", err)
	}

	// 12. Resume VM — thaw-agent wakes up, detects runner_id change, re-runs warmup
	log.WithField("runner_id", runnerID).Info("Resuming VM for incremental warmup...")
	if err := vm.Resume(ctx); err != nil {
		vm.Stop()
		fuseDisk.Unmount()
		for _, d := range fuseExtDisks {
			d.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to resume VM: %w", err)
	}

	log.Info("VM restored and resumed for incremental warmup")
	return vm, fuseDisk, fuseExtDisks, uffdHandler, nil
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
	gitToken, gcpAccessToken string,
	commands []snapshot.SnapshotCommand,
	newDrives []snapshot.DriveSpec,
	runnerID string,
	authProxy *authproxy.AuthProxy, authProxyAddr string,
) (*firecracker.VM, *fuse.ChunkedDisk, map[string]*fuse.ChunkedDisk, *uffd.Handler, error) {
	reattachDir := filepath.Join(*outputDir, "reattach")
	if err := os.MkdirAll(reattachDir, 0755); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to create reattach dir: %w", err)
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
		return nil, nil, nil, nil, fmt.Errorf("failed to create chunk store: %w", err)
	}

	// 1. Load parent metadata (for VM state, memory, rootfs)
	parentMeta, err := chunkStore.LoadChunkedMetadata(ctx, *parentWorkloadKey, *parentVersion)
	if err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to load parent metadata: %w", err)
	}
	log.WithFields(logrus.Fields{
		"parent_key":     *parentWorkloadKey,
		"parent_version": *parentVersion,
		"rootfs_chunks":  len(parentMeta.RootfsChunks),
	}).Info("Loaded parent layer metadata for reattach")

	// 2. Load old layer metadata (for extension drives) — optional for init builds
	var oldLayerMeta *snapshot.ChunkedSnapshotMetadata
	if *previousLayerKey != "" && *previousLayerVersion != "" {
		oldLayerMeta, err = chunkStore.LoadChunkedMetadata(ctx, *previousLayerKey, *previousLayerVersion)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to load old layer metadata: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to download parent memory: %w", err)
		}
	} else if len(parentMeta.MemChunks) > 0 {
		log.Info("Will use UFFD lazy loading from parent memory chunks")
		useUFFD = true
	} else {
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("parent snapshot has no memory data")
	}

	// 4. Fetch parent state chunk
	localStatePath := filepath.Join(reattachDir, "snapshot.state")
	if parentMeta.StateHash != "" {
		stateData, err := chunkStore.GetChunk(ctx, parentMeta.StateHash)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to fetch parent vmstate chunk: %w", err)
		}
		if err := os.WriteFile(localStatePath, stateData, 0644); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to write parent vmstate: %w", err)
		}
	}

	// 5. Fetch kernel chunk from parent
	localKernelPath := filepath.Join(reattachDir, "kernel.bin")
	if parentMeta.KernelHash != "" {
		kernelData, err := chunkStore.GetChunk(ctx, parentMeta.KernelHash)
		if err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to fetch parent kernel chunk: %w", err)
		}
		if err := os.WriteFile(localKernelPath, kernelData, 0644); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to write parent kernel: %w", err)
		}
	} else {
		if err := copyFile(*kernelPath, localKernelPath); err != nil {
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to copy kernel: %w", err)
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
		return nil, nil, nil, nil, fmt.Errorf("failed to create FUSE rootfs disk: %w", err)
	}
	if err := fuseDisk.Mount(); err != nil {
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to mount FUSE rootfs: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to create drive %s: %w", d.DriveID, err)
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
		return nil, nil, nil, nil, fmt.Errorf("failed to create snapshot symlink dir: %w", err)
	}
	symlinkRootfs := filepath.Join(snapshotSymlinkDir, "rootfs.img")
	os.Remove(symlinkRootfs)
	if err := os.Symlink(fuseDisk.DiskImagePath(), symlinkRootfs); err != nil {
		cleanupFuse()
		return nil, nil, nil, nil, fmt.Errorf("symlink rootfs: %w", err)
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
		Drives: vmDrives,
	}
	vm, err := firecracker.NewVM(vmCfg, logger)
	if err != nil {
		for _, c := range createdSymlinks {
			os.Remove(c)
		}
		cleanupFuse()
		return nil, nil, nil, nil, fmt.Errorf("failed to create VM: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to create mem chunk store: %w", err)
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
			return nil, nil, nil, nil, fmt.Errorf("failed to create UFFD handler: %w", err)
		}
		if err := uffdHandler.Start(); err != nil {
			memChunkStore.Close()
			cleanupVM()
			return nil, nil, nil, nil, fmt.Errorf("failed to start UFFD handler: %w", err)
		}

		log.Info("Restoring VM from parent snapshot with UFFD (reattach)...")
		if err := vm.RestoreFromSnapshotWithUFFD(ctx, localStatePath, uffdSocketPath, false); err != nil {
			uffdHandler.Stop()
			memChunkStore.Close()
			cleanupVM()
			return nil, nil, nil, nil, fmt.Errorf("failed to restore from parent snapshot with UFFD: %w", err)
		}
	} else {
		log.Info("Restoring VM from parent snapshot (file-backed memory, reattach)...")
		if err := vm.RestoreFromSnapshot(ctx, localStatePath, localMemPath, false); err != nil {
			cleanupVM()
			return nil, nil, nil, nil, fmt.Errorf("failed to restore from parent snapshot: %w", err)
		}
	}

	for _, c := range createdSymlinks {
		os.Remove(c)
	}

	newRunnerID := fmt.Sprintf("snapshot-builder-reattach-%s", uuid.New().String()[:8])
	mmdsData := buildWarmupMMDS(commands, gitToken, newDrives)
	mmdsData["latest"].(map[string]interface{})["meta"].(map[string]interface{})["runner_id"] = newRunnerID
	injectProxyMMDS(mmdsData, authProxy, authProxyAddr, hostIP)

	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		cleanupFuse()
		return nil, nil, nil, nil, fmt.Errorf("failed to set MMDS data: %w", err)
	}

	log.WithField("runner_id", runnerID).Info("Resuming VM for reattach warmup...")
	if err := vm.Resume(ctx); err != nil {
		vm.Stop()
		cleanupFuse()
		return nil, nil, nil, nil, fmt.Errorf("failed to resume VM: %w", err)
	}

	log.Info("VM restored and resumed for reattach warmup")
	return vm, fuseDisk, fuseExtDisks, uffdHandler, nil
}

// buildWarmupMMDS creates the MMDS data for warmup mode.
// commands are passed through to thaw-agent as warmup.commands.
func buildWarmupMMDS(commands []snapshot.SnapshotCommand, gitToken, drives ...[]snapshot.DriveSpec) map[string]interface{} {
	job := map[string]interface{}{}
	if gitToken != "" {
		job["git_token"] = gitToken
	}

	return map[string]interface{}{
		"latest": map[string]interface{}{
			"meta": map[string]interface{}{
				"mode":        "warmup",
				"runner_id":   "snapshot-builder",
				"environment": "snapshot-build",
			},
			"warmup": func() map[string]interface{} {
				w := map[string]interface{}{"commands": commands}
				if len(drives) > 0 && len(drives[0]) > 0 {
					w["drives"] = drives[0]
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

// injectProxyMMDS adds auth proxy metadata to the MMDS data if an auth proxy is running.
// The thaw-agent reads this to configure HTTPS_PROXY, GCE_METADATA_HOST, and install the CA certificate.
func injectProxyMMDS(mmdsData map[string]interface{}, proxy *authproxy.AuthProxy, proxyAddr, metadataHost string) {
	if proxy == nil {
		return
	}
	latest := mmdsData["latest"].(map[string]interface{})
	latest["proxy"] = map[string]interface{}{
		"ca_cert_pem":   string(proxy.CACertPEM),
		"address":       proxyAddr,
		"metadata_host": metadataHost,
	}
}

// buildRootfsFromImage creates a Firecracker-compatible ext4 rootfs from a Docker image.
// Steps:
//  1. Pull the Docker image
//  2. Create a container and export its filesystem
//  3. Create an ext4 image and populate it from the export
//  4. Inject the platform shim (systemd init, thaw-agent, networking)
func buildRootfsFromImage(imageURI, outputPath, runnerUser string, log *logrus.Entry) error {
	log.WithField("image", imageURI).Info("Pulling Docker image...")
	if output, err := exec.Command("docker", "pull", "--platform=linux/amd64", imageURI).CombinedOutput(); err != nil {
		return fmt.Errorf("docker pull failed: %s: %w", string(output), err)
	}

	// Export container filesystem to tar
	log.Info("Exporting container filesystem...")
	containerID, err := exec.Command("docker", "create", "--platform=linux/amd64", imageURI, "/bin/true").Output()
	if err != nil {
		return fmt.Errorf("docker create failed: %w", err)
	}
	cid := strings.TrimSpace(string(containerID))
	defer exec.Command("docker", "rm", cid).Run()

	tarPath := outputPath + ".tar"
	tarFile, err := os.Create(tarPath)
	if err != nil {
		return fmt.Errorf("failed to create tar file: %w", err)
	}

	exportCmd := exec.Command("docker", "export", cid)
	exportCmd.Stdout = tarFile
	if err := exportCmd.Run(); err != nil {
		tarFile.Close()
		return fmt.Errorf("docker export failed: %w", err)
	}
	tarFile.Close()
	defer os.Remove(tarPath)

	// Create ext4 image (8GB default, same as production rootfs)
	rootfsSizeGB := 8
	log.WithField("size_gb", rootfsSizeGB).Info("Creating ext4 rootfs image...")
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", rootfsSizeGB), outputPath).Run(); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}
	if output, err := exec.Command("mkfs.ext4", "-F", outputPath).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}

	// Mount and populate
	mountDir := outputPath + ".mnt"
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	if output, err := exec.Command("mount", "-o", "loop", outputPath, mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %s: %w", string(output), err)
	}
	defer exec.Command("umount", mountDir).Run()

	// Extract container filesystem
	log.Info("Extracting container filesystem into rootfs...")
	if output, err := exec.Command("tar", "xf", tarPath, "-C", mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("tar extract failed: %s: %w", string(output), err)
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
	log.Info("Injecting platform shim (systemd, thaw-agent, networking)...")
	if err := injectPlatformShim(mountDir, runnerUser, dockerEnv, log); err != nil {
		return fmt.Errorf("platform shim injection failed: %w", err)
	}

	log.Info("Rootfs built from Docker image successfully")
	return nil
}

// injectPlatformShim installs the minimal components needed to run a Firecracker
// microVM on top of any Docker image: systemd init, thaw-agent, network config.
// dockerEnv contains ENV variables extracted from the Docker image config
// (e.g., ["PATH=/opt/venv/bin:/usr/bin", "HOME=/home/user"]) that are written
// to /etc/environment so all processes in the VM inherit them.
func injectPlatformShim(rootfsDir, runnerUser string, dockerEnv []string, log *logrus.Entry) error {
	// 1. Ensure systemd is installed (check for /lib/systemd/systemd)
	systemdPath := filepath.Join(rootfsDir, "lib/systemd/systemd")
	if _, err := os.Stat(systemdPath); os.IsNotExist(err) {
		// Install systemd via chroot apt-get (requires the image to be Debian/Ubuntu-based)
		log.Info("Installing systemd in rootfs...")
		installCmd := exec.Command("chroot", rootfsDir, "/bin/sh", "-c",
			"apt-get update -qq && apt-get install -y -qq --no-install-recommends systemd systemd-sysv dbus iproute2 sudo")
		installCmd.Env = []string{"DEBIAN_FRONTEND=noninteractive", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
		if output, err := installCmd.CombinedOutput(); err != nil {
			log.WithField("output", string(output)).Warn("systemd install failed (image may not be Debian-based)")
			return fmt.Errorf("systemd installation failed: %w", err)
		}
	}

	// 2. Create /init symlink to systemd
	initLink := filepath.Join(rootfsDir, "init")
	os.Remove(initLink) // remove if exists
	if err := os.Symlink("/lib/systemd/systemd", initLink); err != nil {
		// Try /sbin/init fallback
		os.Symlink("/lib/systemd/systemd", filepath.Join(rootfsDir, "sbin/init"))
	}

	// 3. Copy thaw-agent binary (statically linked, available at /usr/local/bin/thaw-agent on the builder VM)
	thawAgentSrc := "/usr/local/bin/thaw-agent"
	thawAgentDst := filepath.Join(rootfsDir, "usr/local/bin/thaw-agent")
	os.MkdirAll(filepath.Dir(thawAgentDst), 0755)
	if err := copyFile(thawAgentSrc, thawAgentDst); err != nil {
		return fmt.Errorf("failed to copy thaw-agent: %w", err)
	}
	os.Chmod(thawAgentDst, 0755)

	// 4. Write thaw-agent systemd service
	serviceDir := filepath.Join(rootfsDir, "etc/systemd/system")
	os.MkdirAll(serviceDir, 0755)
	serviceContent := fmt.Sprintf(`[Unit]
Description=Thaw Agent - MicroVM initialization
After=network.target
Wants=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/thaw-agent --sandbox-user=%s
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal
Environment=LOG_LEVEL=info
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
`, runnerUser)
	if err := os.WriteFile(filepath.Join(serviceDir, "thaw-agent.service"), []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write thaw-agent.service: %w", err)
	}
	// Enable the service
	wantsDir := filepath.Join(serviceDir, "multi-user.target.wants")
	os.MkdirAll(wantsDir, 0755)
	os.Symlink("/etc/systemd/system/thaw-agent.service", filepath.Join(wantsDir, "thaw-agent.service"))

	// 5. Write network config (static IP, configured at runtime by thaw-agent)
	networkDir := filepath.Join(rootfsDir, "etc/systemd/network")
	os.MkdirAll(networkDir, 0755)
	networkContent := `[Match]
Name=eth0

[Network]
DHCP=no
`
	os.WriteFile(filepath.Join(networkDir, "10-eth0.network"), []byte(networkContent), 0644)

	// 6. Configure systemd defaults
	exec.Command("chroot", rootfsDir, "systemctl", "set-default", "multi-user.target").Run()
	exec.Command("chroot", rootfsDir, "systemctl", "mask",
		"systemd-resolved.service", "systemd-timesyncd.service").Run()

	// 7. Create required directories
	for _, dir := range []string{
		"workspace",
		"var/run/thaw-agent",
		"var/log/thaw-agent",
		"mnt/ephemeral/caches/repository",
		"mnt/ephemeral/bazel",
		"mnt/ephemeral/output",
	} {
		os.MkdirAll(filepath.Join(rootfsDir, dir), 0755)
	}

	// 8. Create runner user if it doesn't exist (no sudo access by default).
	// The runner user runs user-level commands (git clone, builds, etc.).
	// Only thaw-agent (running as root via systemd) and commands explicitly
	// marked with run_as_root: true get root privileges.
	exec.Command("chroot", rootfsDir, "useradd", "-m", "-s", "/bin/bash", runnerUser).Run()
	// Ensure the runner user owns its workspace
	exec.Command("chroot", rootfsDir, "chown", "-R", runnerUser+":"+runnerUser, "/workspace").Run()

	// 9. Set hostname, DNS defaults, and /etc/hosts.
	// Docker bind-mounts /etc/hosts at runtime, so `docker export` produces
	// an empty file. We must write a proper one or localhost won't resolve,
	// breaking health checks and any loopback connections.
	os.WriteFile(filepath.Join(rootfsDir, "etc/hostname"), []byte("runner\n"), 0644)
	os.WriteFile(filepath.Join(rootfsDir, "etc/hosts"), []byte("127.0.0.1\tlocalhost\n::1\t\tlocalhost ip6-localhost ip6-loopback\n"), 0644)
	os.WriteFile(filepath.Join(rootfsDir, "etc/resolv.conf.default"), []byte("nameserver 8.8.8.8\n"), 0644)

	// Fix nsswitch.conf to use files+dns instead of systemd-resolve (which is masked).
	// Many Docker images ship with "hosts: files resolve [!UNAVAIL=return] dns" which
	// breaks getent/glibc resolution when systemd-resolved is not running.
	nsswitchPath := filepath.Join(rootfsDir, "etc/nsswitch.conf")
	if nssData, err := os.ReadFile(nsswitchPath); err == nil {
		fixed := strings.ReplaceAll(string(nssData), "resolve [!UNAVAIL=return]", "")
		fixed = strings.ReplaceAll(fixed, "resolve", "")
		os.WriteFile(nsswitchPath, []byte(fixed), 0644)
	}

	// 10. Enable serial console for Firecracker
	exec.Command("chroot", rootfsDir, "systemctl", "enable", "serial-getty@ttyS0.service").Run()

	// 11. Write Docker image ENV variables to /etc/environment so all processes
	// (including start_command, login shells, etc.) inherit them. Docker ENV
	// directives are image metadata, not filesystem content, so they're lost
	// during docker export → ext4 conversion. We restore them here.
	if len(dockerEnv) > 0 {
		var envLines []string
		for _, env := range dockerEnv {
			// Skip Docker-internal vars that don't make sense in a VM context
			if strings.HasPrefix(env, "DEBIAN_FRONTEND=") {
				continue
			}
			envLines = append(envLines, env)
		}
		if len(envLines) > 0 {
			envContent := strings.Join(envLines, "\n") + "\n"
			envPath := filepath.Join(rootfsDir, "etc/environment")
			if err := os.WriteFile(envPath, []byte(envContent), 0644); err != nil {
				log.WithError(err).Warn("Failed to write /etc/environment")
			} else {
				log.WithField("env_count", len(envLines)).Info("Wrote Docker ENV to /etc/environment")
			}

			// Also write a profile.d script so interactive shells source them.
			// /etc/environment is read by PAM but not by non-login shells;
			// the profile.d script covers bash -c invocations used by thaw-agent.
			profileDir := filepath.Join(rootfsDir, "etc/profile.d")
			os.MkdirAll(profileDir, 0755)
			var profileLines []string
			for _, env := range envLines {
				if idx := strings.Index(env, "="); idx > 0 {
					key := env[:idx]
					val := env[idx+1:]
					profileLines = append(profileLines, fmt.Sprintf("export %s=%q", key, val))
				}
			}
			profileContent := "# Docker image environment variables\n" + strings.Join(profileLines, "\n") + "\n"
			profilePath := filepath.Join(profileDir, "docker-env.sh")
			if err := os.WriteFile(profilePath, []byte(profileContent), 0755); err != nil {
				log.WithError(err).Warn("Failed to write /etc/profile.d/docker-env.sh")
			}
		}
	}

	log.WithField("runner_user", runnerUser).Info("Platform shim injected successfully")
	return nil
}
