package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

var (
	// GCP settings
	project      = flag.String("project", "", "GCP project ID (required)")
	zone         = flag.String("zone", "us-central1-a", "GCP zone for disk operations")
	region       = flag.String("region", "us-central1", "GCP region for snapshot storage")
	diskSizeGB   = flag.Int("disk-size-gb", 200, "Size of data disk in GB")
	diskType     = flag.String("disk-type", "pd-ssd", "Disk type (pd-ssd, pd-balanced, pd-standard)")

	// Source artifacts
	snapshotGCSPath = flag.String("snapshot-gcs", "", "GCS path to Firecracker snapshot artifacts (gs://bucket/current/)")
	snapshotLocalPath = flag.String("snapshot-local", "", "Local path to snapshot artifacts (alternative to GCS)")

	// Git cache settings
	repos        = flag.String("repos", "", "Comma-separated repo specs: github.com/org/repo:dirname,...")
	githubToken  = flag.String("github-token", "", "GitHub token for private repos (or GITHUB_TOKEN env)")
	gitCacheSize = flag.Int("git-cache-img-size-gb", 100, "Size of git-cache.img in GB")

	// Snapshot naming
	snapshotPrefix = flag.String("snapshot-prefix", "runner-data", "Prefix for snapshot names")
	snapshotLabel  = flag.String("snapshot-label", "current", "Label value for current snapshot")
	
	// GCS for metadata (for freshness checker)
	metadataBucket = flag.String("metadata-bucket", "", "GCS bucket for metadata upload (for freshness checker)")

	// Operational flags
	keepDisk   = flag.Bool("keep-disk", false, "Don't delete the build disk after snapshot (for debugging)")
	dryRun     = flag.Bool("dry-run", false, "Build disk but don't create snapshot")
	logLevel   = flag.String("log-level", "info", "Log level")
)

// SnapshotMetadata records what's in the snapshot
type SnapshotMetadata struct {
	Version       string               `json:"version"`
	BuildTime     time.Time            `json:"build_time"`
	SnapshotName  string               `json:"snapshot_name"`
	DiskSizeGB    int                  `json:"disk_size_gb"`
	SnapshotSource string              `json:"snapshot_source"`
	Repos         map[string]RepoInfo  `json:"repos"`
}

type RepoInfo struct {
	URL       string    `json:"url"`
	CommitSHA string    `json:"commit_sha"`
	Branch    string    `json:"branch"`
	ClonedAt  time.Time `json:"cloned_at"`
}

func main() {
	flag.Parse()

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	level, _ := logrus.ParseLevel(*logLevel)
	logger.SetLevel(level)
	log := logger.WithField("component", "data-snapshot-builder")

	if *project == "" {
		log.Fatal("--project is required")
	}
	if *snapshotGCSPath == "" && *snapshotLocalPath == "" {
		log.Fatal("Either --snapshot-gcs or --snapshot-local is required")
	}

	token := *githubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}

	ctx := context.Background()
	version := time.Now().Format("20060102-150405")
	diskName := fmt.Sprintf("%s-build-%s", *snapshotPrefix, version)
	snapshotName := fmt.Sprintf("%s-%s", *snapshotPrefix, version)

	log.WithFields(logrus.Fields{
		"project":  *project,
		"zone":     *zone,
		"disk":     diskName,
		"snapshot": snapshotName,
	}).Info("Starting data snapshot build")

	// Step 1: Create disk
	log.Info("Creating build disk...")
	if err := createDisk(ctx, *project, *zone, diskName, *diskSizeGB, *diskType); err != nil {
		log.WithError(err).Fatal("Failed to create disk")
	}
	defer func() {
		if !*keepDisk {
			log.Info("Cleaning up build disk...")
			deleteDisk(ctx, *project, *zone, diskName, log)
		}
	}()

	// Step 2: Attach disk to this instance
	hostname, _ := os.Hostname()
	log.WithField("hostname", hostname).Info("Attaching disk to this instance...")
	if err := attachDisk(ctx, *project, *zone, hostname, diskName); err != nil {
		log.WithError(err).Fatal("Failed to attach disk")
	}
	defer detachDisk(ctx, *project, *zone, hostname, diskName, log)

	// Wait for device to appear
	devicePath := fmt.Sprintf("/dev/disk/by-id/google-%s", diskName)
	log.WithField("device", devicePath).Info("Waiting for device...")
	if err := waitForDevice(devicePath, 60*time.Second); err != nil {
		log.WithError(err).Fatal("Device did not appear")
	}

	// Step 3: Format and mount
	mountPoint := "/mnt/snapshot-build"
	log.Info("Formatting and mounting disk...")
	if err := formatAndMount(devicePath, mountPoint); err != nil {
		log.WithError(err).Fatal("Failed to format/mount disk")
	}
	defer unmount(mountPoint, log)

	// Step 4: Populate disk
	metadata := &SnapshotMetadata{
		Version:      version,
		BuildTime:    time.Now(),
		SnapshotName: snapshotName,
		DiskSizeGB:   *diskSizeGB,
		Repos:        make(map[string]RepoInfo),
	}

	// 4a: Copy snapshot artifacts
	snapshotsDir := filepath.Join(mountPoint, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0755); err != nil {
		log.WithError(err).Fatal("Failed to create snapshots dir")
	}

	if *snapshotGCSPath != "" {
		log.WithField("source", *snapshotGCSPath).Info("Copying snapshot artifacts from GCS...")
		metadata.SnapshotSource = *snapshotGCSPath
		if err := copyFromGCS(ctx, *snapshotGCSPath, snapshotsDir); err != nil {
			log.WithError(err).Fatal("Failed to copy snapshot artifacts")
		}
	} else {
		log.WithField("source", *snapshotLocalPath).Info("Copying snapshot artifacts from local...")
		metadata.SnapshotSource = *snapshotLocalPath
		if err := copyLocal(*snapshotLocalPath, snapshotsDir); err != nil {
			log.WithError(err).Fatal("Failed to copy snapshot artifacts")
		}
	}

	// 4b: Clone git repos
	if *repos != "" {
		gitCacheDir := filepath.Join(mountPoint, "git-cache")
		if err := os.MkdirAll(gitCacheDir, 0755); err != nil {
			log.WithError(err).Fatal("Failed to create git-cache dir")
		}

		repoList := parseRepos(*repos)
		for _, repo := range repoList {
			repoLog := log.WithFields(logrus.Fields{"repo": repo.URL, "name": repo.Name})
			repoLog.Info("Cloning repository...")

			info, err := cloneRepo(repo, gitCacheDir, token)
			if err != nil {
				repoLog.WithError(err).Error("Failed to clone repo")
				continue
			}
			metadata.Repos[repo.Name] = *info
			repoLog.WithField("commit", info.CommitSHA).Info("Repository cloned")
		}

		// 4c: Create git-cache.img block device
		gitCacheImg := filepath.Join(mountPoint, "git-cache.img")
		log.Info("Creating git-cache.img block device...")
		if err := createGitCacheImage(gitCacheImg, gitCacheDir, *gitCacheSize); err != nil {
			log.WithError(err).Fatal("Failed to create git-cache.img")
		}
	}

	// 4d: Write metadata
	metadataPath := filepath.Join(mountPoint, "metadata.json")
	metadataBytes, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(metadataPath, metadataBytes, 0644); err != nil {
		log.WithError(err).Warn("Failed to write metadata")
	}

	// Sync filesystem
	exec.Command("sync").Run()
	log.Info("Disk populated successfully")

	// Step 5: Unmount before snapshot
	if err := unmountForSnapshot(mountPoint); err != nil {
		log.WithError(err).Fatal("Failed to unmount disk")
	}

	// Step 6: Detach disk before snapshot
	log.Info("Detaching disk before snapshot...")
	if err := detachDiskSync(ctx, *project, *zone, hostname, diskName); err != nil {
		log.WithError(err).Fatal("Failed to detach disk")
	}

	if *dryRun {
		log.Info("Dry-run mode, skipping snapshot creation")
		return
	}

	// Step 7: Create snapshot
	log.Info("Creating disk snapshot...")
	if err := createSnapshot(ctx, *project, *zone, *region, diskName, snapshotName); err != nil {
		log.WithError(err).Fatal("Failed to create snapshot")
	}

	// Step 8: Update labels (mark as current)
	log.Info("Updating snapshot labels...")
	if err := updateSnapshotLabels(ctx, *project, *snapshotPrefix, snapshotName, *snapshotLabel); err != nil {
		log.WithError(err).Warn("Failed to update labels")
	}

	// Step 9: Upload metadata to GCS (for freshness checker)
	if *metadataBucket != "" {
		log.Info("Uploading metadata to GCS for freshness checker...")
		metadataGCS := fmt.Sprintf("gs://%s/data-snapshot/current/metadata.json", *metadataBucket)
		if err := uploadMetadataToGCS(ctx, metadataBytes, metadataGCS); err != nil {
			log.WithError(err).Warn("Failed to upload metadata to GCS")
		} else {
			log.WithField("gcs_path", metadataGCS).Info("Metadata uploaded to GCS")
		}
	}

	log.WithFields(logrus.Fields{
		"snapshot": snapshotName,
		"project":  *project,
	}).Info("Data snapshot created successfully!")

	fmt.Printf("\nSnapshot created: %s\n", snapshotName)
	fmt.Printf("To use in Terraform:\n")
	fmt.Printf("  use_data_snapshot = true\n")
	fmt.Printf("  data_snapshot_name = \"%s\"\n", snapshotName)
}

func createDisk(ctx context.Context, project, zone, name string, sizeGB int, diskType string) error {
	args := []string{
		"compute", "disks", "create", name,
		"--project", project,
		"--zone", zone,
		"--size", fmt.Sprintf("%dGB", sizeGB),
		"--type", diskType,
	}
	cmd := exec.CommandContext(ctx, "gcloud", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gcloud disks create failed: %s: %w", string(output), err)
	}
	return nil
}

func deleteDisk(ctx context.Context, project, zone, name string, log *logrus.Entry) {
	cmd := exec.CommandContext(ctx, "gcloud", "compute", "disks", "delete", name,
		"--project", project, "--zone", zone, "--quiet")
	if output, err := cmd.CombinedOutput(); err != nil {
		log.WithError(err).WithField("output", string(output)).Warn("Failed to delete disk")
	}
}

func attachDisk(ctx context.Context, project, zone, instance, disk string) error {
	cmd := exec.CommandContext(ctx, "gcloud", "compute", "instances", "attach-disk", instance,
		"--disk", disk,
		"--device-name", disk,
		"--project", project,
		"--zone", zone)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("attach-disk failed: %s: %w", string(output), err)
	}
	return nil
}

func detachDisk(ctx context.Context, project, zone, instance, disk string, log *logrus.Entry) {
	cmd := exec.CommandContext(ctx, "gcloud", "compute", "instances", "detach-disk", instance,
		"--disk", disk,
		"--project", project,
		"--zone", zone)
	if output, err := cmd.CombinedOutput(); err != nil {
		log.WithError(err).WithField("output", string(output)).Warn("Failed to detach disk")
	}
}

func detachDiskSync(ctx context.Context, project, zone, instance, disk string) error {
	cmd := exec.CommandContext(ctx, "gcloud", "compute", "instances", "detach-disk", instance,
		"--disk", disk,
		"--project", project,
		"--zone", zone)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("detach-disk failed: %s: %w", string(output), err)
	}
	return nil
}

func waitForDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("device %s did not appear within %v", path, timeout)
}

func formatAndMount(device, mountPoint string) error {
	// Format
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", "RUNNER_DATA", device).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}

	// Create mount point
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return err
	}

	// Mount
	if output, err := exec.Command("mount", device, mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %s: %w", string(output), err)
	}

	return nil
}

func unmount(mountPoint string, log *logrus.Entry) {
	exec.Command("sync").Run()
	if output, err := exec.Command("umount", mountPoint).CombinedOutput(); err != nil {
		log.WithError(err).WithField("output", string(output)).Warn("Failed to unmount")
	}
}

func unmountForSnapshot(mountPoint string) error {
	exec.Command("sync").Run()
	time.Sleep(2 * time.Second) // Give filesystem time to flush
	if output, err := exec.Command("umount", mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("umount failed: %s: %w", string(output), err)
	}
	return nil
}

func copyFromGCS(ctx context.Context, gcsPath, destDir string) error {
	// Ensure trailing slash for rsync-like behavior
	if !strings.HasSuffix(gcsPath, "/") {
		gcsPath += "/"
	}
	cmd := exec.CommandContext(ctx, "gsutil", "-m", "rsync", "-r", gcsPath, destDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gsutil rsync failed: %s: %w", string(output), err)
	}
	return nil
}

func copyLocal(srcDir, destDir string) error {
	cmd := exec.Command("cp", "-a", srcDir+"/.", destDir+"/")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp failed: %s: %w", string(output), err)
	}
	return nil
}

type repoSpec struct {
	URL  string
	Name string
}

func parseRepos(reposFlag string) []repoSpec {
	var specs []repoSpec
	for _, spec := range strings.Split(reposFlag, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		parts := strings.SplitN(spec, ":", 2)
		url := strings.TrimSpace(parts[0])
		name := filepath.Base(url)
		if len(parts) == 2 {
			name = strings.TrimSpace(parts[1])
		}
		specs = append(specs, repoSpec{URL: url, Name: name})
	}
	return specs
}

func cloneRepo(repo repoSpec, baseDir, token string) (*RepoInfo, error) {
	targetDir := filepath.Join(baseDir, repo.Name)

	cloneURL := "https://" + repo.URL
	if token != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@%s", token, repo.URL)
	}

	os.RemoveAll(targetDir)

	cmd := exec.Command("git", "clone", cloneURL, targetDir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone failed: %s: %w", string(output), err)
	}

	// Fetch all branches
	exec.Command("git", "-C", targetDir, "fetch", "--all").Run()

	// Get commit SHA
	commitOut, _ := exec.Command("git", "-C", targetDir, "rev-parse", "HEAD").Output()
	branchOut, _ := exec.Command("git", "-C", targetDir, "rev-parse", "--abbrev-ref", "HEAD").Output()

	return &RepoInfo{
		URL:       repo.URL,
		CommitSHA: strings.TrimSpace(string(commitOut)),
		Branch:    strings.TrimSpace(string(branchOut)),
		ClonedAt:  time.Now(),
	}, nil
}

func createGitCacheImage(imagePath, sourceDir string, sizeGB int) error {
	// Create sparse image
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", sizeGB), imagePath).Run(); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}

	// Create ext4 filesystem
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", "GIT_CACHE", imagePath).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}

	// Mount and copy
	mountPoint := filepath.Join(filepath.Dir(imagePath), "mnt-git-cache-tmp")
	os.MkdirAll(mountPoint, 0755)
	defer os.RemoveAll(mountPoint)

	if output, err := exec.Command("mount", "-o", "loop", imagePath, mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %s: %w", string(output), err)
	}
	defer exec.Command("umount", mountPoint).Run()

	// Copy repos into image
	entries, _ := os.ReadDir(sourceDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		src := filepath.Join(sourceDir, entry.Name())
		dst := filepath.Join(mountPoint, entry.Name())
		if output, err := exec.Command("cp", "-a", src, dst).CombinedOutput(); err != nil {
			return fmt.Errorf("cp failed: %s: %w", string(output), err)
		}
	}

	exec.Command("chmod", "-R", "755", mountPoint).Run()
	exec.Command("sync").Run()

	return nil
}

func createSnapshot(ctx context.Context, project, zone, region, diskName, snapshotName string) error {
	cmd := exec.CommandContext(ctx, "gcloud", "compute", "disks", "snapshot", diskName,
		"--project", project,
		"--zone", zone,
		"--snapshot-names", snapshotName,
		"--storage-location", region)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("snapshot failed: %s: %w", string(output), err)
	}
	return nil
}

func uploadMetadataToGCS(ctx context.Context, metadata []byte, gcsPath string) error {
	// Write to temp file
	tmpFile, err := os.CreateTemp("", "metadata-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	
	if _, err := tmpFile.Write(metadata); err != nil {
		return err
	}
	tmpFile.Close()
	
	cmd := exec.CommandContext(ctx, "gsutil", "cp", tmpFile.Name(), gcsPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gsutil cp failed: %s: %w", string(output), err)
	}
	return nil
}

func updateSnapshotLabels(ctx context.Context, project, prefix, newSnapshot, labelValue string) error {
	// Find existing "current" snapshot and remove label
	listCmd := exec.CommandContext(ctx, "gcloud", "compute", "snapshots", "list",
		"--project", project,
		"--filter", fmt.Sprintf("labels.%s=true AND name~^%s", labelValue, prefix),
		"--format", "value(name)")
	output, _ := listCmd.Output()
	
	oldSnapshots := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, old := range oldSnapshots {
		old = strings.TrimSpace(old)
		if old != "" && old != newSnapshot {
			exec.CommandContext(ctx, "gcloud", "compute", "snapshots", "remove-labels", old,
				"--project", project,
				"--labels", labelValue).Run()
		}
	}

	// Add label to new snapshot
	cmd := exec.CommandContext(ctx, "gcloud", "compute", "snapshots", "add-labels", newSnapshot,
		"--project", project,
		"--labels", fmt.Sprintf("%s=true", labelValue))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("add-labels failed: %s: %w", string(output), err)
	}

	return nil
}
