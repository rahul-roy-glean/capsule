package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/tiers"
)

// Snapshot represents a snapshot version
type Snapshot struct {
	Version      string
	Status       string
	GCSPath      string
	BazelVersion string
	RepoCommit   string
	SizeBytes    int64
	CreatedAt    time.Time
	Metrics      SnapshotMetrics
}

// SnapshotMetrics holds performance metrics for a snapshot
type SnapshotMetrics struct {
	AvgAnalysisTimeMs int     `json:"avg_analysis_time_ms"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	SampleCount       int     `json:"sample_count"`
}

// SnapshotManager manages snapshot lifecycle
type SnapshotManager struct {
	db             *sql.DB
	gcsClient      *storage.Client
	gcsBucket      string
	gcsPrefix      string // top-level prefix for all GCS paths (e.g. "v1")
	gcpProject     string
	gcpZone        string
	builderImage   string // GCE image for snapshot builder VM
	builderNetwork string // VPC network for builder VM
	logger         *logrus.Entry
	mu             sync.RWMutex
	currentVersion string
}

// NewSnapshotManager creates a new snapshot manager
func NewSnapshotManager(ctx context.Context, db *sql.DB, gcsBucket, gcsPrefix, gcpProject, gcpZone string, logger *logrus.Logger) *SnapshotManager {
	client, err := storage.NewClient(ctx)
	if err != nil {
		logger.WithError(err).Warn("Failed to create GCS client")
	}

	sm := &SnapshotManager{
		db:         db,
		gcsClient:  client,
		gcsBucket:  gcsBucket,
		gcsPrefix:  gcsPrefix,
		gcpProject: gcpProject,
		gcpZone:    gcpZone,
		logger:     logger.WithField("component", "snapshot-manager"),
	}

	// Load current active snapshot version
	if s, err := sm.GetCurrentSnapshot(ctx); err == nil {
		sm.currentVersion = s.Version
	}

	return sm
}

// gcsPath prepends the configured GCS prefix to a path.
func (sm *SnapshotManager) gcsPath(path string) string {
	if sm.gcsPrefix != "" {
		return sm.gcsPrefix + "/" + path
	}
	return path
}

// GetCurrentVersion returns the current active snapshot version
func (sm *SnapshotManager) GetCurrentVersion() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.currentVersion
}

// TriggerBuild is an alias for TriggerSnapshotBuild
func (sm *SnapshotManager) TriggerBuild(ctx context.Context, repo, branch, bazelVersion string) (string, error) {
	return sm.TriggerSnapshotBuild(ctx, repo, branch, bazelVersion)
}

// GetCurrentSnapshot returns the current active snapshot
func (sm *SnapshotManager) GetCurrentSnapshot(ctx context.Context) (*Snapshot, error) {
	var s Snapshot
	var metricsJSON sql.NullString

	err := sm.db.QueryRowContext(ctx, `
		SELECT version, status, gcs_path, bazel_version, repo_commit, size_bytes, created_at, metrics
		FROM snapshots
		WHERE status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`).Scan(&s.Version, &s.Status, &s.GCSPath, &s.BazelVersion, &s.RepoCommit,
		&s.SizeBytes, &s.CreatedAt, &metricsJSON)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no active snapshot")
	}
	if err != nil {
		return nil, err
	}

	if metricsJSON.Valid {
		json.Unmarshal([]byte(metricsJSON.String), &s.Metrics)
	}

	return &s, nil
}

// GetSnapshot returns a specific snapshot
func (sm *SnapshotManager) GetSnapshot(ctx context.Context, version string) (*Snapshot, error) {
	var s Snapshot
	var metricsJSON sql.NullString

	err := sm.db.QueryRowContext(ctx, `
		SELECT version, status, gcs_path, bazel_version, repo_commit, size_bytes, created_at, metrics
		FROM snapshots
		WHERE version = $1
	`, version).Scan(&s.Version, &s.Status, &s.GCSPath, &s.BazelVersion, &s.RepoCommit,
		&s.SizeBytes, &s.CreatedAt, &metricsJSON)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("snapshot not found: %s", version)
	}
	if err != nil {
		return nil, err
	}

	if metricsJSON.Valid {
		json.Unmarshal([]byte(metricsJSON.String), &s.Metrics)
	}

	return &s, nil
}

// ListSnapshots returns all snapshots
func (sm *SnapshotManager) ListSnapshots(ctx context.Context) ([]*Snapshot, error) {
	rows, err := sm.db.QueryContext(ctx, `
		SELECT version, status, gcs_path, bazel_version, repo_commit, size_bytes, created_at, metrics
		FROM snapshots
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snapshots []*Snapshot
	for rows.Next() {
		var s Snapshot
		var metricsJSON sql.NullString

		err := rows.Scan(&s.Version, &s.Status, &s.GCSPath, &s.BazelVersion, &s.RepoCommit,
			&s.SizeBytes, &s.CreatedAt, &metricsJSON)
		if err != nil {
			return nil, err
		}

		if metricsJSON.Valid {
			json.Unmarshal([]byte(metricsJSON.String), &s.Metrics)
		}

		snapshots = append(snapshots, &s)
	}

	return snapshots, nil
}

// CreateSnapshot creates a new snapshot record
func (sm *SnapshotManager) CreateSnapshot(ctx context.Context, s *Snapshot) error {
	metricsJSON, _ := json.Marshal(s.Metrics)

	_, err := sm.db.ExecContext(ctx, `
		INSERT INTO snapshots (version, status, gcs_path, bazel_version, repo_commit, size_bytes, metrics)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, s.Version, s.Status, s.GCSPath, s.BazelVersion, s.RepoCommit, s.SizeBytes, string(metricsJSON))

	return err
}

// UpdateSnapshotStatus updates a snapshot's status
func (sm *SnapshotManager) UpdateSnapshotStatus(ctx context.Context, version, status string) error {
	_, err := sm.db.ExecContext(ctx, `
		UPDATE snapshots SET status = $2 WHERE version = $1
	`, version, status)
	return err
}

// SetActiveSnapshot sets a snapshot as active and deprecates others
func (sm *SnapshotManager) SetActiveSnapshot(ctx context.Context, version string) error {
	tx, err := sm.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Deprecate current active
	_, err = tx.ExecContext(ctx, `
		UPDATE snapshots SET status = 'deprecated' WHERE status = 'active'
	`)
	if err != nil {
		return err
	}

	// Set new active
	_, err = tx.ExecContext(ctx, `
		UPDATE snapshots SET status = 'active' WHERE version = $1
	`, version)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// RecordSnapshotMetrics records performance metrics for a snapshot
func (sm *SnapshotManager) RecordSnapshotMetrics(ctx context.Context, version string, analysisTimeMs int, cacheHitRatio float64) error {
	// Get current metrics
	var metricsJSON sql.NullString
	err := sm.db.QueryRowContext(ctx, `
		SELECT metrics FROM snapshots WHERE version = $1
	`, version).Scan(&metricsJSON)
	if err != nil {
		return err
	}

	var metrics SnapshotMetrics
	if metricsJSON.Valid {
		json.Unmarshal([]byte(metricsJSON.String), &metrics)
	}

	// Update running average
	metrics.SampleCount++
	metrics.AvgAnalysisTimeMs = (metrics.AvgAnalysisTimeMs*(metrics.SampleCount-1) + analysisTimeMs) / metrics.SampleCount
	metrics.CacheHitRatio = (metrics.CacheHitRatio*float64(metrics.SampleCount-1) + cacheHitRatio) / float64(metrics.SampleCount)

	newMetricsJSON, _ := json.Marshal(metrics)

	_, err = sm.db.ExecContext(ctx, `
		UPDATE snapshots SET metrics = $2 WHERE version = $1
	`, version, string(newMetricsJSON))

	return err
}

// ValidateSnapshot tests a new snapshot by allocating a test runner and verifying health.
func (sm *SnapshotManager) ValidateSnapshot(ctx context.Context, version string) error {
	sm.logger.WithField("version", version).Info("Validating snapshot before rollout")

	// Verify snapshot exists and is in a buildable state
	snapshot, err := sm.GetSnapshot(ctx, version)
	if err != nil {
		return fmt.Errorf("snapshot not found: %w", err)
	}
	if snapshot.Status != "ready" && snapshot.Status != "active" {
		return fmt.Errorf("snapshot not in valid state for validation: status=%s", snapshot.Status)
	}

	// Verify required files exist in GCS
	complete, err := sm.checkSnapshotComplete(ctx, version)
	if err != nil {
		return fmt.Errorf("failed to check snapshot files: %w", err)
	}
	if !complete {
		return fmt.Errorf("snapshot files incomplete in GCS")
	}

	// This is a placeholder for the full validation logic.
	// In production, this would:
	// 1. Pick a healthy host from the registry
	// 2. Send a SyncSnapshot RPC to have it pull the new version
	// 3. Allocate a test runner using the new snapshot
	// 4. Wait for thaw-agent health endpoint to respond
	// 5. Verify Bazel server is running (query /health endpoint)
	// 6. Release the test runner
	// 7. Return nil if all checks pass

	sm.logger.WithField("version", version).Info("Snapshot validation passed (basic check)")
	return nil
}

// SnapshotBuilderConfig holds configuration for launching snapshot builder
type SnapshotBuilderConfig struct {
	GCPProject     string
	GCPZone        string
	MachineType    string
	ImageFamily    string
	ImageProject   string
	Network        string
	Subnet         string
	ServiceAccount string
}

// TriggerSnapshotBuild triggers a new snapshot build
func (sm *SnapshotManager) TriggerSnapshotBuild(ctx context.Context, repo, branch, bazelVersion string) (string, error) {
	branchShort := branch
	if len(branchShort) > 8 {
		branchShort = branch[:8]
	}
	version := fmt.Sprintf("v%s-%s", time.Now().Format("20060102-150405"), branchShort)

	sm.logger.WithFields(logrus.Fields{
		"version": version,
		"repo":    repo,
		"branch":  branch,
	}).Info("Triggering snapshot build")

	// Create snapshot record
	s := &Snapshot{
		Version:      version,
		Status:       "building",
		GCSPath:      fmt.Sprintf("gs://%s/%s/", sm.gcsBucket, sm.gcsPath(version)),
		BazelVersion: bazelVersion,
		CreatedAt:    time.Now(),
	}

	if err := sm.CreateSnapshot(ctx, s); err != nil {
		return "", err
	}

	// Launch snapshot builder VM
	instanceName := fmt.Sprintf("snapshot-builder-%s", version)
	if err := sm.launchSnapshotBuilderVM(ctx, instanceName, repo, branch, bazelVersion, version, "//..."); err != nil {
		sm.UpdateSnapshotStatus(ctx, version, "failed")
		return "", fmt.Errorf("failed to launch snapshot builder: %w", err)
	}

	// Monitor build in background
	go sm.monitorSnapshotBuild(context.Background(), version, instanceName)

	return version, nil
}

// launchSnapshotBuilderVM creates a GCE instance to build a snapshot
func (sm *SnapshotManager) launchSnapshotBuilderVM(ctx context.Context, instanceName, repo, branch, bazelVersion, version, fetchTargets string) error {
	if sm.gcpProject == "" {
		sm.logger.Warn("GCP project not configured, skipping VM launch")
		return nil
	}

	sm.logger.WithFields(logrus.Fields{
		"instance": instanceName,
		"repo":     repo,
		"branch":   branch,
		"version":  version,
	}).Info("Launching snapshot builder VM")

	// Create Compute Engine client
	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create compute client: %w", err)
	}
	defer instancesClient.Close()

	// Build startup script that runs snapshot-builder
	gcsBase := sm.gcsPath("build-artifacts")
	startupScript := fmt.Sprintf(`#!/bin/bash
set -e

# Log everything
exec > >(tee /var/log/snapshot-builder.log) 2>&1

echo "Starting snapshot builder..."
echo "Repo: %s"
echo "Branch: %s"
echo "Version: %s"

# Install dependencies (if not in image)
apt-get update -qq
apt-get install -y -qq git

# Download snapshot-builder binary (should be in the image or GCS)
if [ ! -f /usr/local/bin/snapshot-builder ]; then
    gcloud storage cp gs://%s/%s/snapshot-builder /usr/local/bin/snapshot-builder
    chmod +x /usr/local/bin/snapshot-builder
fi

# Run snapshot builder
/usr/local/bin/snapshot-builder \
    --repo-url="%s" \
    --repo-branch="%s" \
    --bazel-version="%s" \
    --gcs-bucket="%s" \
    --gcs-prefix="%s" \
    --fetch-targets="%s" \
    --output-dir=/tmp/snapshot \
    --log-level=info
	--vcpus=2 \
	--memory-mb=1024 \

# Signal completion
echo "Snapshot build complete, shutting down..."
shutdown -h now
`, repo, branch, version, sm.gcsBucket, gcsBase, repo, branch, bazelVersion, sm.gcsBucket, sm.gcsPrefix, fetchTargets)

	// Configure the instance
	machineType := fmt.Sprintf("zones/%s/machineTypes/n2-standard-8", sm.gcpZone)
	sourceImage := fmt.Sprintf("projects/%s/global/images/family/%s", sm.gcpProject, "firecracker-host")
	if sm.builderImage != "" {
		sourceImage = sm.builderImage
	}

	network := sm.builderNetwork
	if network == "" {
		network = "default"
	}
	networkURL := fmt.Sprintf("projects/%s/global/networks/%s", sm.gcpProject, network)

	req := &computepb.InsertInstanceRequest{
		Project: sm.gcpProject,
		Zone:    sm.gcpZone,
		InstanceResource: &computepb.Instance{
			Name:        proto.String(instanceName),
			MachineType: proto.String(machineType),
			Disks: []*computepb.AttachedDisk{
				{
					InitializeParams: &computepb.AttachedDiskInitializeParams{
						DiskSizeGb:  proto.Int64(100),
						SourceImage: proto.String(sourceImage),
					},
					AutoDelete: proto.Bool(true),
					Boot:       proto.Bool(true),
				},
			},
			NetworkInterfaces: []*computepb.NetworkInterface{
				{
					Network: proto.String(networkURL),
					AccessConfigs: []*computepb.AccessConfig{
						{
							Name: proto.String("External NAT"),
							Type: proto.String("ONE_TO_ONE_NAT"),
						},
					},
				},
			},
			Metadata: &computepb.Metadata{
				Items: []*computepb.Items{
					{
						Key:   proto.String("startup-script"),
						Value: proto.String(startupScript),
					},
					{
						Key:   proto.String("snapshot-version"),
						Value: proto.String(version),
					},
				},
			},
			Labels: map[string]string{
				"purpose":          "snapshot-builder",
				"snapshot-version": version,
			},
			ServiceAccounts: []*computepb.ServiceAccount{
				{
					Email: proto.String("default"),
					Scopes: []string{
						"https://www.googleapis.com/auth/cloud-platform",
					},
				},
			},
			Scheduling: &computepb.Scheduling{
				Preemptible: proto.Bool(false),
			},
		},
	}

	op, err := instancesClient.Insert(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create instance: %w", err)
	}

	// Wait for operation to complete
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("instance creation failed: %w", err)
	}

	sm.logger.WithField("instance", instanceName).Info("Snapshot builder VM created")
	return nil
}

// launchSnapshotBuilderVMForKey creates a GCE instance to build a snapshot from commands JSON.
// When incremental is true, the builder passes --incremental to the snapshot-builder binary
// and runs on a spot (preemptible) instance. Full builds use on-demand instances.
func (sm *SnapshotManager) launchSnapshotBuilderVMForKey(ctx context.Context, instanceName, workloadKey, commandsJSON, version, githubAppID, githubAppSecret string, incremental bool, incrementalCommandsJSON string, snapshotVCPUs, snapshotMemoryMB int) error {
	if sm.gcpProject == "" {
		sm.logger.Warn("GCP project not configured, skipping VM launch")
		return nil
	}

	sm.logger.WithFields(logrus.Fields{
		"instance":     instanceName,
		"workload_key": workloadKey,
		"version":      version,
	}).Info("Launching snapshot builder VM")

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create compute client: %w", err)
	}
	defer instancesClient.Close()

	// Build optional flags
	githubFlags := ""
	if githubAppID != "" && githubAppSecret != "" {
		githubFlags = fmt.Sprintf(`--github-app-id="%s" --github-app-secret="%s" --gcp-project="%s"`,
			githubAppID, githubAppSecret, sm.gcpProject)
	}
	gcsBase := sm.gcsPath("build-artifacts")
	incrementalFlag := ""
	if incremental {
		incrementalFlag = "--incremental"
	}
	incrementalCommandsFlag := ""
	if incrementalCommandsJSON != "" {
		incrementalCommandsFlag = fmt.Sprintf("--incremental-commands='%s'", incrementalCommandsJSON)
	}

	startupScript := fmt.Sprintf(`#!/bin/bash
set -e
exec > >(tee /var/log/snapshot-builder.log) 2>&1
echo "Starting snapshot builder setup..."

# Install Firecracker
ARCH=$(uname -m)
FC_VERSION="1.14.2"
echo "Installing Firecracker v${FC_VERSION}..."
cd /tmp
curl -fSL "https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/firecracker-v${FC_VERSION}-${ARCH}.tgz" -o firecracker.tgz
tar xzf firecracker.tgz
mv "release-v${FC_VERSION}-${ARCH}/firecracker-v${FC_VERSION}-${ARCH}" /usr/local/bin/firecracker
chmod +x /usr/local/bin/firecracker
rm -rf firecracker.tgz "release-v${FC_VERSION}-${ARCH}"

# Setup KVM
modprobe kvm_intel || modprobe kvm_amd || true
chmod 666 /dev/kvm || true

# Download kernel and rootfs from GCS
echo "Downloading kernel and rootfs..."
mkdir -p /opt/firecracker
gcloud storage cp "gs://%s/%s/kernel.bin" /opt/firecracker/kernel.bin 2>/dev/null \
    || echo "WARNING: kernel.bin not found"
gcloud storage cp "gs://%s/%s/%s/rootfs.img" /opt/firecracker/rootfs.img 2>/dev/null \
    || gcloud storage cp "gs://%s/%s/rootfs.img" /opt/firecracker/rootfs.img 2>/dev/null \
    || echo "WARNING: rootfs.img not found"

# Download snapshot-builder binary
if [ ! -f /usr/local/bin/snapshot-builder ]; then
    gcloud storage cp gs://%s/%s/snapshot-builder /usr/local/bin/snapshot-builder
    chmod +x /usr/local/bin/snapshot-builder
fi

# Run snapshot builder
/usr/local/bin/snapshot-builder \
    --snapshot-commands='%s' \
    --gcs-bucket="%s" \
    --gcs-prefix="%s" \
    --output-dir=/tmp/snapshot \
    --log-level=info \
	--vcpus=%d \
	--memory-mb=%d \
    --version="%s" \
    %s %s %s
echo "Snapshot build complete, shutting down..."
shutdown -h now
`, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, workloadKey, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, commandsJSON, sm.gcsBucket, sm.gcsPrefix, snapshotVCPUs, snapshotMemoryMB, version, githubFlags, incrementalFlag, incrementalCommandsFlag)

	// Size the builder VM to fit the snapshot build: give it at least 8 vCPUs
	// or the snapshot's vCPUs + 2 headroom, whichever is larger.
	builderVCPUs := 8
	if snapshotVCPUs+2 > builderVCPUs {
		builderVCPUs = snapshotVCPUs + 2
	}
	machineType := fmt.Sprintf("zones/%s/machineTypes/n2-standard-%d", sm.gcpZone, builderVCPUs)
	sourceImage := fmt.Sprintf("projects/%s/global/images/family/%s", sm.gcpProject, "firecracker-host")
	if sm.builderImage != "" {
		sourceImage = sm.builderImage
	}
	network := sm.builderNetwork
	if network == "" {
		network = "default"
	}
	networkURL := fmt.Sprintf("projects/%s/global/networks/%s", sm.gcpProject, network)

	req := &computepb.InsertInstanceRequest{
		Project: sm.gcpProject,
		Zone:    sm.gcpZone,
		InstanceResource: &computepb.Instance{
			Name:        proto.String(instanceName),
			MachineType: proto.String(machineType),
			AdvancedMachineFeatures: &computepb.AdvancedMachineFeatures{
				EnableNestedVirtualization: proto.Bool(true),
			},
			Disks: []*computepb.AttachedDisk{
				{
					InitializeParams: &computepb.AttachedDiskInitializeParams{
						DiskSizeGb:  proto.Int64(100),
						SourceImage: proto.String(sourceImage),
					},
					AutoDelete: proto.Bool(true),
					Boot:       proto.Bool(true),
				},
			},
			NetworkInterfaces: []*computepb.NetworkInterface{
				{
					Network: proto.String(networkURL),
					AccessConfigs: []*computepb.AccessConfig{
						{
							Name: proto.String("External NAT"),
							Type: proto.String("ONE_TO_ONE_NAT"),
						},
					},
				},
			},
			Metadata: &computepb.Metadata{
				Items: []*computepb.Items{
					{Key: proto.String("startup-script"), Value: proto.String(startupScript)},
					{Key: proto.String("snapshot-version"), Value: proto.String(version)},
				},
			},
			Labels: map[string]string{
				"purpose":          "snapshot-builder",
				"snapshot-version": version,
			},
			ServiceAccounts: []*computepb.ServiceAccount{
				{
					Email:  proto.String("default"),
					Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
				},
			},
			Scheduling: &computepb.Scheduling{
				// Incremental builds are fast and cheap to retry — use spot instances.
				// Full builds are long-running — use on-demand instances.
				Preemptible: proto.Bool(incremental),
			},
		},
	}

	op, err := instancesClient.Insert(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create instance: %w", err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("instance creation failed: %w", err)
	}

	sm.logger.WithField("instance", instanceName).Info("Snapshot builder VM created")
	return nil
}

// monitorSnapshotBuild monitors the snapshot build progress
func (sm *SnapshotManager) monitorSnapshotBuild(ctx context.Context, version, instanceName string) {
	sm.logger.WithFields(logrus.Fields{
		"version":  version,
		"instance": instanceName,
	}).Info("Monitoring snapshot build")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	timeout := time.After(45 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			sm.logger.WithField("version", version).Error("Snapshot build timed out")
			sm.UpdateSnapshotStatus(ctx, version, "failed")
			sm.cleanupBuilderVM(ctx, instanceName)
			return
		case <-ticker.C:
			// Check if snapshot files exist in GCS
			complete, err := sm.checkSnapshotComplete(ctx, version)
			if err != nil {
				sm.logger.WithError(err).Debug("Error checking snapshot completion")
				continue
			}

			if complete {
				sm.logger.WithField("version", version).Info("Snapshot build completed")
				sm.UpdateSnapshotStatus(ctx, version, "ready")
				sm.cleanupBuilderVM(ctx, instanceName)
				return
			}

			// Check if VM is still running
			if sm.gcpProject != "" {
				running, err := sm.isBuilderVMRunning(ctx, instanceName)
				if err != nil {
					sm.logger.WithError(err).Debug("Error checking VM status")
					continue
				}
				if !running {
					// VM terminated without completing - check if snapshot exists
					complete, _ := sm.checkSnapshotComplete(ctx, version)
					if complete {
						sm.UpdateSnapshotStatus(ctx, version, "ready")
					} else {
						sm.logger.WithField("version", version).Error("Builder VM terminated without completing snapshot")
						sm.UpdateSnapshotStatus(ctx, version, "failed")
					}
					return
				}
			}
		}
	}
}

// checkSnapshotComplete checks if snapshot files exist in GCS.
// It uses the gcs_path stored in the DB record to find the files.
func (sm *SnapshotManager) checkSnapshotComplete(ctx context.Context, version string) (bool, error) {
	bucket := sm.gcsClient.Bucket(sm.gcsBucket)

	// Look up the GCS prefix from the snapshot record — it may be repo-scoped
	// (e.g. "org-repo/v20260221-..." instead of "v20260221-...")
	gcsPrefix := version
	var gcsPath sql.NullString
	if err := sm.db.QueryRow(`SELECT gcs_path FROM snapshots WHERE version = $1`, version).Scan(&gcsPath); err == nil && gcsPath.Valid {
		// gcs_path is "gs://bucket/org-repo/v20260221-.../", extract the prefix after bucket
		prefix := strings.TrimPrefix(gcsPath.String, fmt.Sprintf("gs://%s/", sm.gcsBucket))
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix != "" {
			gcsPrefix = prefix
		}
	}

	requiredFiles := []string{
		fmt.Sprintf("%s/kernel.bin", gcsPrefix),
		fmt.Sprintf("%s/rootfs.img", gcsPrefix),
		fmt.Sprintf("%s/metadata.json", gcsPrefix),
	}

	for _, file := range requiredFiles {
		_, err := bucket.Object(file).Attrs(ctx)
		if err != nil {
			return false, nil // File doesn't exist yet
		}
	}

	return true, nil
}

// isBuilderVMRunning checks if the builder VM is still running
func (sm *SnapshotManager) isBuilderVMRunning(ctx context.Context, instanceName string) (bool, error) {
	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return false, err
	}
	defer instancesClient.Close()

	instance, err := instancesClient.Get(ctx, &computepb.GetInstanceRequest{
		Project:  sm.gcpProject,
		Zone:     sm.gcpZone,
		Instance: instanceName,
	})
	if err != nil {
		return false, err
	}

	status := instance.GetStatus()
	return status == "RUNNING" || status == "STAGING", nil
}

// cleanupBuilderVM deletes the snapshot builder VM
func (sm *SnapshotManager) cleanupBuilderVM(ctx context.Context, instanceName string) {
	if sm.gcpProject == "" {
		return
	}

	sm.logger.WithField("instance", instanceName).Info("Cleaning up builder VM")

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		sm.logger.WithError(err).Warn("Failed to create compute client for cleanup")
		return
	}
	defer instancesClient.Close()

	op, err := instancesClient.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project:  sm.gcpProject,
		Zone:     sm.gcpZone,
		Instance: instanceName,
	})
	if err != nil {
		sm.logger.WithError(err).Warn("Failed to delete builder VM")
		return
	}

	if err := op.Wait(ctx); err != nil {
		sm.logger.WithError(err).Warn("Failed waiting for VM deletion")
	}
}

// FreshnessCheckLoop periodically checks snapshot freshness
func (sm *SnapshotManager) FreshnessCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.checkFreshness(ctx)
		}
	}
}

func (sm *SnapshotManager) checkFreshness(ctx context.Context) {
	current, err := sm.GetCurrentSnapshot(ctx)
	if err != nil {
		sm.logger.WithError(err).Warn("Failed to get current snapshot")
		return
	}

	age := time.Since(current.CreatedAt)
	sm.logger.WithFields(logrus.Fields{
		"version": current.Version,
		"age":     age,
	}).Debug("Checking snapshot freshness")

	// Check if snapshot is too old
	if age > 24*time.Hour {
		sm.logger.WithField("version", current.Version).Warn("Snapshot is stale (>24h)")
		// Could trigger automatic rebuild here
	}

	// Check if cache hit ratio has degraded
	if current.Metrics.SampleCount > 10 && current.Metrics.CacheHitRatio < 0.5 {
		sm.logger.WithFields(logrus.Fields{
			"version":         current.Version,
			"cache_hit_ratio": current.Metrics.CacheHitRatio,
		}).Warn("Cache hit ratio degraded")
	}
}

// SnapshotToProto converts a Snapshot to its proto representation
func (sm *SnapshotManager) SnapshotToProto(s *Snapshot) *pb.Snapshot {
	if s == nil {
		return nil
	}
	return &pb.Snapshot{
		Version:      s.Version,
		Status:       s.Status,
		GcsPath:      s.GCSPath,
		BazelVersion: s.BazelVersion,
		RepoCommit:   s.RepoCommit,
		SizeBytes:    s.SizeBytes,
		CreatedAt:    timestamppb.New(s.CreatedAt),
	}
}

// RolloutConfig holds configuration for snapshot rollout
type RolloutConfig struct {
	CanaryPercent    int           // Percentage of hosts for canary (default 10)
	CanaryWaitTime   time.Duration // Time to wait observing canary before full rollout (default 5m)
	RolloutBatchSize int           // Number of hosts to rollout at once (default 5)
	HealthCheckURL   string        // URL path to check host health after sync
}

// RolloutSnapshot rolls out a new snapshot to hosts
func (sm *SnapshotManager) RolloutSnapshot(ctx context.Context, version string, hostRegistry *HostRegistry) error {
	return sm.RolloutSnapshotWithConfig(ctx, version, hostRegistry, RolloutConfig{
		CanaryPercent:    10,
		CanaryWaitTime:   5 * time.Minute,
		RolloutBatchSize: 5,
	})
}

// RolloutSnapshotWithConfig rolls out a snapshot with custom configuration
func (sm *SnapshotManager) RolloutSnapshotWithConfig(ctx context.Context, version string, hostRegistry *HostRegistry, cfg RolloutConfig) error {
	sm.logger.WithField("version", version).Info("Rolling out snapshot")

	// Verify snapshot is ready
	snapshot, err := sm.GetSnapshot(ctx, version)
	if err != nil {
		return fmt.Errorf("snapshot not found: %w", err)
	}
	if snapshot.Status != "ready" && snapshot.Status != "active" {
		return fmt.Errorf("snapshot not ready: status=%s", snapshot.Status)
	}

	// Get all healthy hosts
	hosts := hostRegistry.GetAvailableHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no healthy hosts available")
	}

	// Calculate canary count
	canaryPercent := cfg.CanaryPercent
	if canaryPercent <= 0 {
		canaryPercent = 10
	}
	canaryCount := (len(hosts) * canaryPercent) / 100
	if canaryCount < 1 {
		canaryCount = 1
	}
	if canaryCount > len(hosts) {
		canaryCount = len(hosts)
	}

	sm.logger.WithFields(logrus.Fields{
		"version":      version,
		"canary_count": canaryCount,
		"total_hosts":  len(hosts),
	}).Info("Starting canary rollout")

	// Phase 1: Canary rollout
	canaryHosts := hosts[:canaryCount]
	remainingHosts := hosts[canaryCount:]

	if err := sm.rolloutToHosts(ctx, version, canaryHosts); err != nil {
		return fmt.Errorf("canary rollout failed: %w", err)
	}

	// Wait and monitor canary hosts
	sm.logger.WithField("wait_time", cfg.CanaryWaitTime).Info("Monitoring canary hosts...")
	if err := sm.monitorCanaryHosts(ctx, canaryHosts, cfg.CanaryWaitTime); err != nil {
		sm.logger.WithError(err).Error("Canary monitoring failed, aborting rollout")
		// Could trigger rollback here
		return fmt.Errorf("canary monitoring failed: %w", err)
	}

	sm.logger.Info("Canary successful, proceeding with full rollout")

	// Phase 2: Full rollout in batches
	batchSize := cfg.RolloutBatchSize
	if batchSize <= 0 {
		batchSize = 5
	}

	for i := 0; i < len(remainingHosts); i += batchSize {
		end := i + batchSize
		if end > len(remainingHosts) {
			end = len(remainingHosts)
		}
		batch := remainingHosts[i:end]

		sm.logger.WithFields(logrus.Fields{
			"batch":     i/batchSize + 1,
			"hosts":     len(batch),
			"remaining": len(remainingHosts) - end,
		}).Info("Rolling out to batch")

		if err := sm.rolloutToHosts(ctx, version, batch); err != nil {
			sm.logger.WithError(err).Warn("Batch rollout had errors, continuing...")
		}

		// Brief pause between batches
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}

	// Mark snapshot as active
	if err := sm.SetActiveSnapshot(ctx, version); err != nil {
		return fmt.Errorf("failed to set active snapshot: %w", err)
	}

	sm.mu.Lock()
	sm.currentVersion = version
	sm.mu.Unlock()

	sm.logger.WithField("version", version).Info("Snapshot rollout complete")
	return nil
}

// rolloutToHosts signals hosts to sync the new snapshot version
func (sm *SnapshotManager) rolloutToHosts(ctx context.Context, version string, hosts []*Host) error {
	var errors []error

	for _, host := range hosts {
		if host.HTTPAddress == "" {
			sm.logger.WithField("host", host.InstanceName).Warn("Host has no HTTP address, skipping")
			continue
		}

		if err := sm.signalHostToSync(ctx, host, version); err != nil {
			sm.logger.WithError(err).WithField("host", host.InstanceName).Warn("Failed to signal host")
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 && len(errors) == len(hosts) {
		return fmt.Errorf("all hosts failed to sync")
	}

	return nil
}

// signalHostToSync sends a request to a host to sync the new snapshot
func (sm *SnapshotManager) signalHostToSync(ctx context.Context, host *Host, version string) error {
	url := fmt.Sprintf("http://%s/snapshot/sync", host.HTTPAddress)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Snapshot-Version", version)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("sync request failed: status %d", resp.StatusCode)
	}

	sm.logger.WithFields(logrus.Fields{
		"host":    host.InstanceName,
		"version": version,
	}).Debug("Signaled host to sync snapshot")

	return nil
}

// monitorCanaryHosts monitors canary hosts for a period to detect issues
func (sm *SnapshotManager) monitorCanaryHosts(ctx context.Context, hosts []*Host, duration time.Duration) error {
	deadline := time.Now().Add(duration)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	failureThreshold := 3
	failures := make(map[string]int)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for _, host := range hosts {
				healthy, err := sm.checkHostHealth(ctx, host)
				if err != nil || !healthy {
					failures[host.ID]++
					sm.logger.WithFields(logrus.Fields{
						"host":     host.InstanceName,
						"failures": failures[host.ID],
					}).Warn("Canary host health check failed")

					if failures[host.ID] >= failureThreshold {
						return fmt.Errorf("canary host %s exceeded failure threshold", host.InstanceName)
					}
				} else {
					failures[host.ID] = 0 // Reset on success
				}
			}
		}
	}

	return nil
}

// checkHostHealth checks if a host is healthy
func (sm *SnapshotManager) checkHostHealth(ctx context.Context, host *Host) (bool, error) {
	if host.HTTPAddress == "" {
		return false, fmt.Errorf("no HTTP address")
	}

	url := fmt.Sprintf("http://%s/health", host.HTTPAddress)
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

// --- Repo-scoped methods (Phase 1.6) ---

// GetCurrentSnapshotForKey returns the current active snapshot for a specific repo.
func (sm *SnapshotManager) GetCurrentSnapshotForKey(ctx context.Context, workloadKey string) (*Snapshot, error) {
	var s Snapshot
	var metricsJSON sql.NullString

	err := sm.db.QueryRowContext(ctx, `
		SELECT version, status, gcs_path, bazel_version, repo_commit, size_bytes, created_at, metrics
		FROM snapshots
		WHERE workload_key = $1 AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`, workloadKey).Scan(&s.Version, &s.Status, &s.GCSPath, &s.BazelVersion, &s.RepoCommit,
		&s.SizeBytes, &s.CreatedAt, &metricsJSON)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no active snapshot for repo %s", workloadKey)
	}
	if err != nil {
		return nil, err
	}

	if metricsJSON.Valid {
		json.Unmarshal([]byte(metricsJSON.String), &s.Metrics)
	}

	return &s, nil
}

// GetCurrentVersionForKey returns the current active version for a specific repo.
func (sm *SnapshotManager) GetCurrentVersionForKey(workloadKey string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	// Fall back to the global version if no workload key given
	if workloadKey == "" {
		return sm.currentVersion
	}
	// Query DB for repo-specific current version
	var version sql.NullString
	err := sm.db.QueryRow(`
		SELECT version FROM snapshots
		WHERE workload_key = $1 AND status = 'active'
		ORDER BY created_at DESC LIMIT 1
	`, workloadKey).Scan(&version)
	if err != nil || !version.Valid {
		return ""
	}
	return version.String
}

// ListSnapshotsForKey returns snapshots filtered by repo slug.
func (sm *SnapshotManager) ListSnapshotsForKey(ctx context.Context, workloadKey string) ([]*Snapshot, error) {
	rows, err := sm.db.QueryContext(ctx, `
		SELECT version, status, gcs_path, bazel_version, repo_commit, size_bytes, created_at, metrics
		FROM snapshots
		WHERE workload_key = $1
		ORDER BY created_at DESC
	`, workloadKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snapshots []*Snapshot
	for rows.Next() {
		var s Snapshot
		var metricsJSON sql.NullString

		err := rows.Scan(&s.Version, &s.Status, &s.GCSPath, &s.BazelVersion, &s.RepoCommit,
			&s.SizeBytes, &s.CreatedAt, &metricsJSON)
		if err != nil {
			return nil, err
		}

		if metricsJSON.Valid {
			json.Unmarshal([]byte(metricsJSON.String), &s.Metrics)
		}

		snapshots = append(snapshots, &s)
	}

	return snapshots, nil
}

// SetActiveSnapshotForKey sets a snapshot as active for a repo, deprecating the previous one.
func (sm *SnapshotManager) SetActiveSnapshotForKey(ctx context.Context, workloadKey, version string) error {
	tx, err := sm.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Deprecate current active for this repo
	_, err = tx.ExecContext(ctx, `
		UPDATE snapshots SET status = 'deprecated'
		WHERE workload_key = $1 AND status = 'active'
	`, workloadKey)
	if err != nil {
		return err
	}

	// Set new active
	_, err = tx.ExecContext(ctx, `
		UPDATE snapshots SET status = 'active'
		WHERE version = $1 AND workload_key = $2
	`, version, workloadKey)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// TriggerSnapshotBuildForKey triggers a snapshot build for a specific workload key.
// When incremental is true, the builder restores from the previous snapshot and
// the VM runs on a spot (preemptible) instance to save costs. Full builds use
// on-demand instances since they are long-running and expensive to retry.
func (sm *SnapshotManager) TriggerSnapshotBuildForKey(ctx context.Context, workloadKey string, commands []snapshot.SnapshotCommand, incrementalCommands []snapshot.SnapshotCommand, githubAppID, githubAppSecret string, incremental bool) (string, error) {
	version := fmt.Sprintf("v%s-%s", time.Now().Format("20060102-150405"), workloadKey)

	sm.logger.WithFields(logrus.Fields{
		"version":      version,
		"workload_key": workloadKey,
		"incremental":  incremental,
	}).Info("Triggering snapshot build for key")

	// Create snapshot record
	metricsJSON, _ := json.Marshal(SnapshotMetrics{})

	commandsJSON, _ := json.Marshal(commands)
	_, err := sm.db.ExecContext(ctx, `
		INSERT INTO snapshots (version, status, gcs_path, bazel_version, repo_commit, size_bytes, metrics, workload_key)
		VALUES ($1, 'building', $2, '', '', 0, $3, $4)
	`, version, fmt.Sprintf("gs://%s/%s/", sm.gcsBucket, sm.gcsPath(workloadKey+"/snapshot_state/"+version)),
		string(metricsJSON), workloadKey)
	if err != nil {
		return "", err
	}

	// Pass incremental commands to the VM when doing an incremental build
	var incrementalCommandsJSON string
	if incremental && len(incrementalCommands) > 0 {
		b, _ := json.Marshal(incrementalCommands)
		incrementalCommandsJSON = string(b)
	}

	// Look up tier for this workload key to determine snapshot vCPUs/memory
	snapshotVCPUs := 4 // default "m" tier
	snapshotMemoryMB := 4096
	if sm.db != nil {
		var tierName sql.NullString
		if err := sm.db.QueryRowContext(ctx, `SELECT tier FROM snapshot_configs WHERE workload_key = $1`, workloadKey).Scan(&tierName); err == nil && tierName.Valid && tierName.String != "" {
			if t, err := tiers.Lookup(tierName.String); err == nil {
				snapshotVCPUs = t.VCPUs
				snapshotMemoryMB = t.MemoryMB
			}
		}
	}

	// Launch snapshot builder VM
	instanceName := fmt.Sprintf("snapshot-builder-%s", version)
	if err := sm.launchSnapshotBuilderVMForKey(ctx, instanceName, workloadKey, string(commandsJSON), version, githubAppID, githubAppSecret, incremental, incrementalCommandsJSON, snapshotVCPUs, snapshotMemoryMB); err != nil {
		sm.UpdateSnapshotStatus(ctx, version, "failed")
		return "", fmt.Errorf("failed to launch snapshot builder: %w", err)
	}

	// Monitor build in background
	go sm.monitorSnapshotBuildForKey(context.Background(), version, instanceName, workloadKey)

	return version, nil
}

// monitorSnapshotBuildForKey monitors a repo-scoped snapshot build.
// After build completes, it auto-validates and optionally auto-rolls out.
func (sm *SnapshotManager) monitorSnapshotBuildForKey(ctx context.Context, version, instanceName, workloadKey string) {
	sm.logger.WithFields(logrus.Fields{
		"version":      version,
		"instance":     instanceName,
		"workload_key": workloadKey,
	}).Info("Monitoring repo snapshot build")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	timeout := time.After(45 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			sm.logger.WithField("version", version).Error("Snapshot build timed out")
			sm.UpdateSnapshotStatus(ctx, version, "failed")
			sm.cleanupBuilderVM(ctx, instanceName)
			return
		case <-ticker.C:
			complete, err := sm.checkSnapshotComplete(ctx, version)
			if err != nil {
				sm.logger.WithError(err).Debug("Error checking snapshot completion")
				continue
			}

			if complete {
				sm.logger.WithField("version", version).Info("Snapshot build completed")
				sm.UpdateSnapshotStatus(ctx, version, "ready")
				sm.cleanupBuilderVM(ctx, instanceName)

				// Auto-validate
				if err := sm.ValidateSnapshot(ctx, version); err != nil {
					sm.logger.WithError(err).Error("Snapshot validation failed")
					sm.UpdateSnapshotStatus(ctx, version, "failed")
					return
				}
				sm.UpdateSnapshotStatus(ctx, version, "validating")

				// Check if auto-rollout is enabled for this workload key
				var autoRollout bool
				err := sm.db.QueryRowContext(ctx, `SELECT auto_rollout FROM snapshot_configs WHERE workload_key = $1`, workloadKey).Scan(&autoRollout)
				if err != nil {
					sm.logger.WithError(err).Warn("Failed to check auto_rollout setting")
					return
				}

				if autoRollout {
					sm.logger.WithField("version", version).Info("Auto-rollout enabled, starting rollout")
					// The rollout pipeline will handle canary → full rollout
					sm.UpdateSnapshotStatus(ctx, version, "canary")
				}
				return
			}

			// Check if VM is still running
			if sm.gcpProject != "" {
				running, err := sm.isBuilderVMRunning(ctx, instanceName)
				if err != nil {
					sm.logger.WithError(err).Debug("Error checking VM status")
					continue
				}
				if !running {
					complete, _ := sm.checkSnapshotComplete(ctx, version)
					if complete {
						sm.UpdateSnapshotStatus(ctx, version, "ready")
					} else {
						sm.logger.WithField("version", version).Error("Builder VM terminated without completing snapshot")
						sm.UpdateSnapshotStatus(ctx, version, "failed")
					}
					return
				}
			}
		}
	}
}

// --- Version Assignment Methods (Phase 4.3) ---

// AssignVersion upserts a version assignment for a repo on a host (or fleet-wide if hostID is nil).
func (sm *SnapshotManager) AssignVersion(ctx context.Context, workloadKey string, hostID *string, version string) error {
	if hostID == nil {
		// Fleet-wide assignment (host_id IS NULL)
		_, err := sm.db.ExecContext(ctx, `
			INSERT INTO version_assignments (workload_key, host_id, version, status)
			VALUES ($1, NULL, $2, 'assigned')
			ON CONFLICT (workload_key, host_id) DO UPDATE SET
				version = EXCLUDED.version,
				status = 'assigned',
				assigned_at = NOW(),
				synced_at = NULL
		`, workloadKey, version)
		return err
	}

	_, err := sm.db.ExecContext(ctx, `
		INSERT INTO version_assignments (workload_key, host_id, version, status)
		VALUES ($1, $2, $3, 'assigned')
		ON CONFLICT (workload_key, host_id) DO UPDATE SET
			version = EXCLUDED.version,
			status = 'assigned',
			assigned_at = NOW(),
			synced_at = NULL
	`, workloadKey, *hostID, version)
	return err
}

// GetDesiredVersions returns the desired snapshot versions for a host,
// combining fleet-wide defaults with per-host overrides.
func (sm *SnapshotManager) GetDesiredVersions(ctx context.Context, hostID string) (map[string]string, error) {
	result := make(map[string]string)

	// First get fleet-wide defaults (host_id IS NULL)
	rows, err := sm.db.QueryContext(ctx, `
		SELECT workload_key, version FROM version_assignments
		WHERE host_id IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var workloadKey, version string
		if err := rows.Scan(&workloadKey, &version); err != nil {
			return nil, err
		}
		result[workloadKey] = version
	}

	// Then apply per-host overrides
	rows, err = sm.db.QueryContext(ctx, `
		SELECT workload_key, version FROM version_assignments
		WHERE host_id = $1
	`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var workloadKey, version string
		if err := rows.Scan(&workloadKey, &version); err != nil {
			return nil, err
		}
		result[workloadKey] = version // Override fleet-wide default
	}

	return result, nil
}

// GetFleetConvergence returns the convergence status for all hosts for a given repo.
type HostVersionStatus struct {
	HostID         string `json:"host_id"`
	InstanceName   string `json:"instance_name"`
	DesiredVersion string `json:"desired_version"`
	CurrentVersion string `json:"current_version"`
	Converged      bool   `json:"converged"`
}

func (sm *SnapshotManager) GetFleetConvergence(ctx context.Context, workloadKey string) ([]HostVersionStatus, error) {
	rows, err := sm.db.QueryContext(ctx, `
		SELECT h.id, h.instance_name, h.snapshot_version,
		       COALESCE(
		           (SELECT va.version FROM version_assignments va WHERE va.workload_key = $1 AND va.host_id = h.id),
		           (SELECT va.version FROM version_assignments va WHERE va.workload_key = $1 AND va.host_id IS NULL),
		           ''
		       ) as desired_version
		FROM hosts h
		WHERE h.status IN ('ready', 'draining')
		ORDER BY h.instance_name
	`, workloadKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []HostVersionStatus
	for rows.Next() {
		var s HostVersionStatus
		var snapshotVersion sql.NullString
		if err := rows.Scan(&s.HostID, &s.InstanceName, &snapshotVersion, &s.DesiredVersion); err != nil {
			return nil, err
		}
		if snapshotVersion.Valid {
			s.CurrentVersion = snapshotVersion.String
		}
		s.Converged = s.CurrentVersion == s.DesiredVersion
		statuses = append(statuses, s)
	}

	return statuses, nil
}

// RollbackSnapshot reverts a repo to its previous active version.
func (sm *SnapshotManager) RollbackSnapshot(ctx context.Context, workloadKey string) error {
	// Find the previous active version (now deprecated)
	var prevVersion string
	err := sm.db.QueryRowContext(ctx, `
		SELECT version FROM snapshots
		WHERE workload_key = $1 AND status = 'deprecated'
		ORDER BY created_at DESC
		LIMIT 1
	`, workloadKey).Scan(&prevVersion)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no previous version to rollback to for repo %s", workloadKey)
	}
	if err != nil {
		return err
	}

	sm.logger.WithFields(logrus.Fields{
		"workload_key": workloadKey,
		"prev_version": prevVersion,
	}).Info("Rolling back to previous version")

	// Mark current active as rolled_back
	_, err = sm.db.ExecContext(ctx, `
		UPDATE snapshots SET status = 'rolled_back'
		WHERE workload_key = $1 AND status = 'active'
	`, workloadKey)
	if err != nil {
		return err
	}

	// Set previous version as active
	if err := sm.SetActiveSnapshotForKey(ctx, workloadKey, prevVersion); err != nil {
		return err
	}

	// Update fleet-wide assignment
	if err := sm.AssignVersion(ctx, workloadKey, nil, prevVersion); err != nil {
		return err
	}

	// Clear per-host overrides
	_, err = sm.db.ExecContext(ctx, `
		DELETE FROM version_assignments
		WHERE workload_key = $1 AND host_id IS NOT NULL
	`, workloadKey)

	return err
}
