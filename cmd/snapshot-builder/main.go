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
	repoCacheUpperSizeGB = flag.Int("repo-cache-upper-size-gb", 10, "Size in GB of repo-cache-upper.img (writable overlay for Bazel repository cache)")
	repoCacheSeedSizeGB  = flag.Int("repo-cache-seed-size-gb", 20, "Size in GB of repo-cache-seed.img (shared Bazel repository cache seed)")
	repoCacheSeedDir     = flag.String("repo-cache-seed-dir", "", "Optional directory to seed into repo-cache-seed.img (copied into image root)")
	gitCachePath         = flag.String("git-cache-path", "", "Path to pre-populated git-cache.img (from git-cache-builder). If set, uses this instead of cloning during warmup.")
	logLevel             = flag.String("log-level", "info", "Log level")
	enableChunked        = flag.Bool("enable-chunked", true, "Also build a chunked snapshot for lazy loading")
	memBackend           = flag.String("mem-backend", "chunked", "Memory backend for chunked snapshots: 'chunked' (UFFD lazy loading via MemChunks, default) or 'file' (upload snapshot.mem as single blob)")
	gcsPrefix            = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")

	incremental     = flag.Bool("incremental", false, "Restore from previous snapshot for incremental rebuild")
	versionOverride = flag.String("version", "", "Snapshot version string (if empty, auto-generated from timestamp + workload key)")

	// Snapshot commands (replaces --repo-slug/--repo-url/--repo-branch/--bazel-version)
	snapshotCommands       = flag.String("snapshot-commands", "", "JSON array of SnapshotCommand describing what to bake into the snapshot (required)")
	incrementalCommandsStr = flag.String("incremental-commands", "", "JSON commands to use for incremental builds (overrides --snapshot-commands when --incremental is set)")

	// GitHub App authentication for private repos (used when git-cache is not available)
	githubAppID     = flag.String("github-app-id", "", "GitHub App ID for private repo access")
	githubAppSecret = flag.String("github-app-secret", "", "GCP Secret Manager secret name containing GitHub App private key")
	gcpProject      = flag.String("gcp-project", "", "GCP project for Secret Manager (defaults to metadata project)")
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
	// Parse incremental commands if provided
	var incrCommands []snapshot.SnapshotCommand
	if *incrementalCommandsStr != "" {
		if err := json.Unmarshal([]byte(*incrementalCommandsStr), &incrCommands); err != nil {
			log.WithError(err).Warn("invalid --incremental-commands, ignoring")
		}
	}

	workloadKey := snapshot.ComputeWorkloadKey(commands)
	log.WithField("workload_key", workloadKey).Info("Computed workload key from snapshot commands")

	// Generate version string (use override from control plane if provided)
	version := *versionOverride
	if version == "" {
		version = fmt.Sprintf("v%s-%s", time.Now().Format("20060102-150405"), workloadKey)
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
	hostIP := "172.16.0.1"
	netmask := "255.255.255.0"

	log.Info("Setting up network for warmup VM...")
	if err := setupWarmupNetwork(tapName, hostIP); err != nil {
		log.WithError(err).Fatal("Failed to setup warmup network")
	}
	defer cleanupWarmupNetwork(tapName)

	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init ip=%s::%s:%s::eth0:off",
		guestIP, hostIP, netmask)

	// Track FUSE disks for cleanup and incremental chunking
	var vm *firecracker.VM
	var fuseDisk *fuse.ChunkedDisk
	var fuseSeedDisk *fuse.ChunkedDisk
	var incrUffdHandler *uffd.Handler

	// Paths used by both paths and for final snapshot creation
	workingRootfs := filepath.Join(*outputDir, "rootfs.img")
	repoCacheSeedImg := filepath.Join(*outputDir, "repo-cache-seed.img")

	// rootfsSourceHash is lazily computed and stored in chunked metadata
	// so future incremental builds can detect rootfs changes.
	var rootfsSourceHash string

	// Try incremental restore from previous snapshot
	if *incremental {
		log.Info("Attempting incremental restore from previous snapshot...")
		var incrementalErr error
		vm, fuseDisk, fuseSeedDisk, incrUffdHandler, incrementalErr = restoreFromPreviousSnapshot(ctx, logger, log, vmID, tapName, guestMAC, bootArgs, gitToken, gcpAccessToken, commands, incrCommands)
		if incrementalErr != nil {
			log.WithError(incrementalErr).Warn("Incremental restore failed, falling back to cold boot")
			vm = nil
			if incrUffdHandler != nil {
				incrUffdHandler.Stop()
				incrUffdHandler = nil
			}
			if fuseDisk != nil {
				fuseDisk.Unmount()
				fuseDisk = nil
			}
			if fuseSeedDisk != nil {
				fuseSeedDisk.Unmount()
				fuseSeedDisk = nil
			}
		}
	}

	// Cold boot path (default or fallback from failed incremental)
	if vm == nil {
		log.Info("Using cold boot path...")

		// Create working rootfs (copy of base, optionally expanded)
		log.Info("Creating working rootfs...")
		if err := copyFile(*rootfsPath, workingRootfs); err != nil {
			log.WithError(err).Fatal("Failed to copy rootfs")
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

		// Create (or seed) shared repo cache seed image
		log.WithFields(logrus.Fields{
			"path":     repoCacheSeedImg,
			"size_gb":  *repoCacheSeedSizeGB,
			"seed_dir": *repoCacheSeedDir,
		}).Info("Creating repo-cache seed image")
		if err := createExt4Image(repoCacheSeedImg, *repoCacheSeedSizeGB, "BAZEL_REPO_SEED"); err != nil {
			log.WithError(err).Fatal("Failed to create repo-cache seed image")
		}
		if *repoCacheSeedDir != "" {
			if err := seedExt4ImageFromDir(repoCacheSeedImg, *repoCacheSeedDir, log); err != nil {
				log.WithError(err).Warn("Failed to seed repo-cache image from directory; continuing with empty seed")
			}
		}

		// Create a placeholder per-VM repo cache upper image
		repoCacheUpperImg := filepath.Join(*outputDir, "repo-cache-upper.img")
		if err := createExt4Image(repoCacheUpperImg, *repoCacheUpperSizeGB, "BAZEL_REPO_UPPER"); err != nil {
			log.WithError(err).Fatal("Failed to create repo-cache upper image")
		}

		// Create a placeholder credentials image
		credentialsImg := filepath.Join(*outputDir, "credentials.img")
		if err := createExt4ImageMB(credentialsImg, 32, "CREDENTIALS"); err != nil {
			log.WithError(err).Fatal("Failed to create credentials image")
		}

		// Create or copy git-cache image
		gitCacheImg := filepath.Join(*outputDir, "git-cache.img")
		gitCacheEnabled := false
		if *gitCachePath != "" {
			log.WithField("source", *gitCachePath).Info("Copying pre-populated git-cache image...")
			if err := copyFile(*gitCachePath, gitCacheImg); err != nil {
				log.WithError(err).Fatal("Failed to copy git-cache image")
			}
			gitCacheEnabled = true
		} else {
			log.Info("Creating placeholder git-cache image...")
			if err := createExt4ImageMB(gitCacheImg, 64, "GIT_CACHE"); err != nil {
				log.WithError(err).Fatal("Failed to create git-cache image")
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
			Drives: []firecracker.Drive{
				{
					DriveID:      "repo_cache_seed",
					PathOnHost:   repoCacheSeedImg,
					IsRootDevice: false,
					IsReadOnly:   false,
				},
				{
					DriveID:      "repo_cache_upper",
					PathOnHost:   repoCacheUpperImg,
					IsRootDevice: false,
					IsReadOnly:   false,
				},
				{
					DriveID:      "credentials",
					PathOnHost:   credentialsImg,
					IsRootDevice: false,
					IsReadOnly:   true,
				},
				{
					DriveID:      "git_cache",
					PathOnHost:   gitCacheImg,
					IsRootDevice: false,
					IsReadOnly:   true,
				},
			},
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

		mmdsData := buildWarmupMMDS(commands, gitToken, gcpAccessToken, gitCacheEnabled)
		if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
			vm.Stop()
			log.WithError(err).Fatal("Failed to set MMDS data")
		}
	}

	// Shared path: wait for warmup, snapshot, upload
	log.Info("Waiting for warmup to complete...")
	warmupCtx, warmupCancel := context.WithTimeout(ctx, *warmupTimeout)
	defer warmupCancel()

	if err := waitForWarmup(warmupCtx, vm, guestIP, log); err != nil {
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
	var incrementalSeedChunks []snapshot.ChunkRef
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
	if fuseSeedDisk != nil {
		log.WithField("dirty_chunks", fuseSeedDisk.DirtyChunkCount()).Info("Saving FUSE seed dirty chunks to chunk store...")
		var err error
		incrementalSeedChunks, err = fuseSeedDisk.SaveDirtyChunks(ctx)
		if err != nil {
			log.WithError(err).Fatal("Failed to save dirty seed chunks")
		}
		fuseSeedDisk.Unmount()
		fuseSeedDisk = nil
	}

	// Copy kernel to output
	kernelOutput := filepath.Join(*outputDir, "kernel.bin")
	if wasIncremental {
		// Incremental: kernel was downloaded to incremental subdir, copy to output
		incrKernel := filepath.Join(*outputDir, "incremental", "kernel.bin")
		if err := copyFile(incrKernel, kernelOutput); err != nil {
			log.WithError(err).Fatal("Failed to copy kernel from incremental dir")
		}
	} else {
		// Cold boot: copy kernel from flag path
		if err := copyFile(*kernelPath, kernelOutput); err != nil {
			log.WithError(err).Fatal("Failed to copy kernel")
		}
	}

	// Get file sizes (some files may not exist in incremental mode)
	var totalSize int64
	for _, f := range []string{kernelOutput, workingRootfs, snapshotPath, memPath, repoCacheSeedImg} {
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
		RepoCacheSeedPath: "repo-cache-seed.img",
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
		log.Info("Chunked mode: skipping legacy full-file upload (rootfs, mem, repo-cache-seed)")
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
				WorkloadKey:      workloadKey,
				Commands:         commands,
				CreatedAt:        time.Now(),
				ChunkSize:        snapshot.DefaultChunkSize,
				RootfsSourceHash: rootfsSourceHash,
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
				memGCSPath := fmt.Sprintf("%s/snapshot_state/%s/snapshot.mem.zst", workloadKey, version)
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

			// Use seed chunks from FUSE if available (as an extension drive)
			if incrementalSeedChunks != nil {
				if chunkedMeta.ExtensionDrives == nil {
					chunkedMeta.ExtensionDrives = make(map[string]snapshot.ExtensionDrive)
				}
				var seedSize int64
				for _, c := range incrementalSeedChunks {
					if end := c.Offset + c.Size; end > seedSize {
						seedSize = end
					}
				}
				chunkedMeta.ExtensionDrives["repo_cache_seed"] = snapshot.ExtensionDrive{
					Chunks:    incrementalSeedChunks,
					ReadOnly:  false,
					SizeBytes: seedSize,
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
			// Cold boot: chunk everything from full files on disk
			legacyDriveSpecs := []snapshot.DriveSpec{}
			legacyDriveImages := map[string]string{}
			if _, err := os.Stat(repoCacheSeedImg); err == nil {
				legacyDriveSpecs = append(legacyDriveSpecs, snapshot.DriveSpec{
					DriveID:  "repo_cache_seed",
					Label:    "BAZEL_REPO_SEED",
					ReadOnly: false,
				})
				legacyDriveImages["repo_cache_seed"] = repoCacheSeedImg
			}
			snapshotPaths := &snapshot.SnapshotPaths{
				Kernel:               kernelOutput,
				Rootfs:               workingRootfs,
				Mem:                  memPath,
				State:                snapshotPath,
				RepoCacheSeed:        repoCacheSeedImg,
				Version:              version,
				ExtensionDriveImages: legacyDriveImages,
			}

			chunkedMeta, err := builder.BuildChunkedSnapshot(ctx, snapshotPaths, legacyDriveSpecs, version, workloadKey)
			if err != nil {
				log.WithError(err).Fatal("Failed to build chunked snapshot")
			}
			chunkedMeta.RootfsSourceHash = rootfsSourceHash
			chunkedMeta.Commands = commands

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
	if err := uploader.UpdateCurrentPointerForRepo(ctx, version, workloadKey); err != nil {
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

// WarmupConfig holds configuration for the warmup phase
type WarmupConfig struct {
	RepoURL       string
	RepoBranch    string
	BazelVersion  string
	WarmupTargets string
}

// WarmupMMDSData is the MMDS data structure for warmup VM
type WarmupMMDSData struct {
	Latest struct {
		Meta struct {
			Mode        string `json:"mode"`
			RunnerID    string `json:"runner_id"`
			Environment string `json:"environment"`
		} `json:"meta"`
		Warmup struct {
			RepoURL       string `json:"repo_url"`
			RepoBranch    string `json:"repo_branch"`
			BazelVersion  string `json:"bazel_version"`
			WarmupTargets string `json:"warmup_targets"`
		} `json:"warmup"`
	} `json:"latest"`
}

func waitForWarmup(ctx context.Context, vm *firecracker.VM, guestIP string, log *logrus.Entry) error {
	// Wait for thaw-agent health endpoint to become available
	// The thaw-agent runs warmup and exposes /health and /warmup-status endpoints

	log.WithField("guest_ip", guestIP).Info("Waiting for warmup to complete...")

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
	incrementalCommands []snapshot.SnapshotCommand,
) (*firecracker.VM, *fuse.ChunkedDisk, *fuse.ChunkedDisk, *uffd.Handler, error) {
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
		"version":       chunkedMeta.Version,
		"rootfs_chunks": len(chunkedMeta.RootfsChunks),
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

	// 6. Mount repo-cache-seed via FUSE (if chunks exist in extension drives)
	var fuseSeedDisk *fuse.ChunkedDisk
	var repoCacheSeedPath string
	if seedExt, ok := chunkedMeta.ExtensionDrives["repo_cache_seed"]; ok && len(seedExt.Chunks) > 0 {
		fuseSeedMountDir := filepath.Join(incrDir, "fuse-seed")
		var totalSeedSize int64
		for _, c := range seedExt.Chunks {
			end := c.Offset + c.Size
			if end > totalSeedSize {
				totalSeedSize = end
			}
		}

		fuseSeedDisk, err = fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
			ChunkStore: chunkStore,
			Chunks:     seedExt.Chunks,
			TotalSize:  totalSeedSize,
			ChunkSize:  chunkedMeta.ChunkSize,
			MountPoint: fuseSeedMountDir,
			Logger:     logger,
		})
		if err != nil {
			fuseDisk.Unmount()
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to create FUSE seed disk: %w", err)
		}
		if err := fuseSeedDisk.Mount(); err != nil {
			fuseDisk.Unmount()
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to mount FUSE seed disk: %w", err)
		}
		repoCacheSeedPath = fuseSeedDisk.DiskImagePath()
		log.Info("Mounted FUSE-backed repo-cache-seed from previous snapshot")
	} else {
		// Create a fresh placeholder
		repoCacheSeedPath = filepath.Join(incrDir, "repo-cache-seed.img")
		if err := createExt4Image(repoCacheSeedPath, *repoCacheSeedSizeGB, "BAZEL_REPO_SEED"); err != nil {
			fuseDisk.Unmount()
			chunkStore.Close()
			return nil, nil, nil, nil, fmt.Errorf("failed to create repo-cache-seed image: %w", err)
		}
	}

	// 7. Create fresh repo-cache-upper, credentials, and git-cache images
	repoCacheUpperPath := filepath.Join(incrDir, "repo-cache-upper.img")
	if err := createExt4Image(repoCacheUpperPath, *repoCacheUpperSizeGB, "BAZEL_REPO_UPPER"); err != nil {
		fuseDisk.Unmount()
		if fuseSeedDisk != nil {
			fuseSeedDisk.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to create repo-cache-upper image: %w", err)
	}

	credentialsPath := filepath.Join(incrDir, "credentials.img")
	if err := createExt4ImageMB(credentialsPath, 32, "CREDENTIALS"); err != nil {
		fuseDisk.Unmount()
		if fuseSeedDisk != nil {
			fuseSeedDisk.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to create credentials image: %w", err)
	}

	gitCachePath := filepath.Join(incrDir, "git-cache.img")
	if err := createExt4ImageMB(gitCachePath, 64, "GIT_CACHE"); err != nil {
		fuseDisk.Unmount()
		if fuseSeedDisk != nil {
			fuseSeedDisk.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to create git-cache image: %w", err)
	}

	// 8. Create symlinks in /tmp/snapshot/ so Firecracker can find drives at baked-in paths
	if err := os.MkdirAll(snapshotSymlinkDir, 0755); err != nil {
		fuseDisk.Unmount()
		if fuseSeedDisk != nil {
			fuseSeedDisk.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to create snapshot symlink dir: %w", err)
	}

	symlinks := []struct {
		name   string
		target string
	}{
		{"rootfs.img", fuseDisk.DiskImagePath()},
		{"repo-cache-seed.img", repoCacheSeedPath},
		{"repo-cache-upper.img", repoCacheUpperPath},
		{"credentials.img", credentialsPath},
		{"git-cache.img", gitCachePath},
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
			if fuseSeedDisk != nil {
				fuseSeedDisk.Unmount()
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
		if fuseSeedDisk != nil {
			fuseSeedDisk.Unmount()
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
			if fuseSeedDisk != nil {
				fuseSeedDisk.Unmount()
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
			if fuseSeedDisk != nil {
				fuseSeedDisk.Unmount()
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
			if fuseSeedDisk != nil {
				fuseSeedDisk.Unmount()
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
			if fuseSeedDisk != nil {
				fuseSeedDisk.Unmount()
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
			if fuseSeedDisk != nil {
				fuseSeedDisk.Unmount()
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
	gitCacheEnabled := false
	// Use incremental commands for MMDS if provided, otherwise fall back to full commands
	mmdsCommands := commands
	if len(incrementalCommands) > 0 {
		mmdsCommands = incrementalCommands
		log.WithField("incremental_commands_count", len(incrementalCommands)).Info("Using incremental commands for MMDS")
	}
	mmdsData := buildWarmupMMDS(mmdsCommands, gitToken, gcpAccessToken, gitCacheEnabled)
	// Override runner_id so thaw-agent detects the change and re-runs warmup
	mmdsData["latest"].(map[string]interface{})["meta"].(map[string]interface{})["runner_id"] = newRunnerID

	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		fuseDisk.Unmount()
		if fuseSeedDisk != nil {
			fuseSeedDisk.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to set MMDS data: %w", err)
	}

	// 12. Resume VM — thaw-agent wakes up, detects runner_id change, re-runs warmup
	log.WithField("runner_id", newRunnerID).Info("Resuming VM for incremental warmup...")
	if err := vm.Resume(ctx); err != nil {
		vm.Stop()
		fuseDisk.Unmount()
		if fuseSeedDisk != nil {
			fuseSeedDisk.Unmount()
		}
		chunkStore.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to resume VM: %w", err)
	}

	log.Info("VM restored and resumed for incremental warmup")
	return vm, fuseDisk, fuseSeedDisk, uffdHandler, nil
}

// buildWarmupMMDS creates the MMDS data for warmup mode.
// commands are passed through to thaw-agent as warmup.commands.
func buildWarmupMMDS(commands []snapshot.SnapshotCommand, gitToken, gcpAccessToken string, gitCacheEnabled bool) map[string]interface{} {
	// Extract repo URL from git-clone command for git_cache mapping (best-effort).
	repoURL := ""
	for _, cmd := range commands {
		if cmd.Type == "git-clone" && len(cmd.Args) > 0 {
			repoURL = cmd.Args[0]
			break
		}
	}
	repoName := filepath.Base(strings.TrimSuffix(repoURL, ".git"))

	repoMappings := map[string]string{}
	if repoURL != "" {
		repoMappings[repoURL] = repoName
	}

	return map[string]interface{}{
		"latest": map[string]interface{}{
			"meta": map[string]interface{}{
				"mode":        "warmup",
				"runner_id":   "snapshot-builder",
				"environment": "snapshot-build",
			},
			"warmup": map[string]interface{}{
				"commands": commands,
			},
			"network": map[string]interface{}{
				"ip":        "172.16.0.2/24",
				"gateway":   "172.16.0.1",
				"netmask":   "255.255.255.0",
				"dns":       "8.8.8.8",
				"interface": "eth0",
			},
			"job": map[string]interface{}{
				"repo":             repoURL,
				"git_token":        gitToken,
				"gcp_access_token": gcpAccessToken,
			},
			"git_cache": map[string]interface{}{
				"enabled":       gitCacheEnabled,
				"mount_path":    "/mnt/git-cache",
				"workspace_dir": "/mnt/ephemeral/workdir",
				"repo_mappings": repoMappings,
			},
		},
	}
}
