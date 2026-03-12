package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/rahul-roy-glean/capsule/api/proto/runner"
)

// Snapshot represents a snapshot version
type Snapshot struct {
	Version    string
	Status     string
	GCSPath    string
	RepoCommit string
	SizeBytes  int64
	CreatedAt  time.Time
	Metrics    SnapshotMetrics
}

// SnapshotMetrics holds performance metrics for a snapshot
type SnapshotMetrics struct {
	AvgAnalysisTimeMs int     `json:"avg_analysis_time_ms"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	SampleCount       int     `json:"sample_count"`
}

// SnapshotManager manages snapshot lifecycle
type SnapshotManager struct {
	db                    *sql.DB
	gcsClient             *storage.Client
	gcsBucket             string
	gcsPrefix             string // top-level prefix for all GCS paths (e.g. "v1")
	gcpProject            string
	gcpZone               string
	builderImage          string // GCE image for snapshot builder VM
	builderNetwork        string // VPC network for builder VM
	builderSubnet         string // VPC subnet for builder VM (required for custom-mode VPCs)
	builderServiceAccount string // GCE service account email for builder VMs
	logger                *logrus.Entry
	mu                    sync.RWMutex
	currentVersion        string
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

// ThawAgentHash returns the MD5 hash of the capsule-thaw-agent binary in GCS.
// Used to include the binary identity in the platform layer hash so that
// deploying a new capsule-thaw-agent invalidates the cached platform layer.
func (sm *SnapshotManager) ThawAgentHash(ctx context.Context) string {
	if sm.gcsClient == nil {
		return ""
	}
	obj := sm.gcsClient.Bucket(sm.gcsBucket).Object(sm.gcsPath("build-artifacts/capsule-thaw-agent"))
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		sm.logger.WithError(err).Debug("Failed to get capsule-thaw-agent GCS attrs (hash will be empty)")
		return ""
	}
	return fmt.Sprintf("%x", attrs.MD5)
}

func (sm *SnapshotManager) builderServiceAccountEmail() string {
	if sm.builderServiceAccount != "" {
		return sm.builderServiceAccount
	}
	return "default"
}

// GetCurrentVersion returns the current active snapshot version
func (sm *SnapshotManager) GetCurrentVersion() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.currentVersion
}

// GetCurrentSnapshot returns the current active snapshot
func (sm *SnapshotManager) GetCurrentSnapshot(ctx context.Context) (*Snapshot, error) {
	var s Snapshot
	var metricsJSON sql.NullString

	err := sm.db.QueryRowContext(ctx, `
		SELECT version, status, gcs_path, repo_commit, size_bytes, created_at, metrics
		FROM snapshots
		WHERE status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`).Scan(&s.Version, &s.Status, &s.GCSPath, &s.RepoCommit,
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
		SELECT version, status, gcs_path, repo_commit, size_bytes, created_at, metrics
		FROM snapshots
		WHERE version = $1
	`, version).Scan(&s.Version, &s.Status, &s.GCSPath, &s.RepoCommit,
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
		SELECT version, status, gcs_path, repo_commit, size_bytes, created_at, metrics
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

		err := rows.Scan(&s.Version, &s.Status, &s.GCSPath, &s.RepoCommit,
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
		INSERT INTO snapshots (version, status, gcs_path, repo_commit, size_bytes, metrics)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, s.Version, s.Status, s.GCSPath, s.RepoCommit, s.SizeBytes, string(metricsJSON))

	return err
}

// UpdateSnapshotStatus updates a snapshot's status
func (sm *SnapshotManager) UpdateSnapshotStatus(ctx context.Context, version, status string) error {
	_, err := sm.db.ExecContext(ctx, `
		UPDATE snapshots SET status = $2 WHERE version = $1
	`, version, status)
	return err
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
	// 4. Wait for capsule-thaw-agent health endpoint to respond
	// 5. Verify Bazel server is running (query /health endpoint)
	// 6. Release the test runner
	// 7. Return nil if all checks pass

	sm.logger.WithField("version", version).Info("Snapshot validation passed (basic check)")
	return nil
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

// checkLayerBuildComplete checks if a layer build is complete by looking for
// the chunked-metadata.json under the layer hash's snapshot_state directory.
// Layer builds use: {gcsPrefix}/{layerHash}/snapshot_state/{version}/chunked-metadata.json
// and update: {gcsPrefix}/{layerHash}/current-pointer.json last.
func (sm *SnapshotManager) checkLayerBuildComplete(ctx context.Context, layerHash, version string) (bool, error) {
	if sm.gcsClient == nil {
		return false, fmt.Errorf("GCS client not available")
	}

	bucket := sm.gcsClient.Bucket(sm.gcsBucket)

	// Check for chunked-metadata.json under the version directory
	metadataPath := sm.gcsPath(fmt.Sprintf("%s/snapshot_state/%s/chunked-metadata.json", layerHash, version))
	if _, err := bucket.Object(metadataPath).Attrs(ctx); err != nil {
		return false, nil
	}

	// Also verify current-pointer.json points to this version (written last by snapshot-builder)
	pointerPath := sm.gcsPath(layerHash + "/current-pointer.json")
	reader, err := bucket.Object(pointerPath).NewReader(ctx)
	if err != nil {
		return false, nil
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return false, nil
	}

	var pointer struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pointer); err != nil {
		return false, nil
	}

	return pointer.Version == version, nil
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
		// Treat 404 (VM not found / already deleted) as "not running"
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NOT_FOUND") || strings.Contains(err.Error(), "notFound") {
			return false, nil
		}
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

// launchSnapshotBuilderVMForKey creates a GCE instance to build a snapshot from commands JSON.
//
//nolint:unused // will be wired up when per-key snapshot builds are enabled
func (sm *SnapshotManager) launchSnapshotBuilderVMForKey(ctx context.Context, instanceName, workloadKey, commandsJSON, version string, buildType string, snapshotVCPUs, snapshotMemoryMB int) error {
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
	gcsBase := sm.gcsPath("build-artifacts")
	buildTypeFlag := ""
	if buildType != "" && buildType != "init" {
		buildTypeFlag = fmt.Sprintf("--build-type=%s", buildType)
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
modprobe tun || true

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
    %s
echo "Snapshot build complete, shutting down..."
shutdown -h now
`, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, workloadKey, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, commandsJSON, sm.gcsBucket, sm.gcsPrefix, snapshotVCPUs, snapshotMemoryMB, version, buildTypeFlag)

	// Size the builder VM to fit the snapshot build: give it at least 8 vCPUs
	// or the snapshot's vCPUs + 2 headroom, whichever is larger.
	builderVCPUs := 8
	if snapshotVCPUs+2 > builderVCPUs {
		builderVCPUs = snapshotVCPUs + 2
	}
	builderVCPUs = nextValidN2VCPUs(builderVCPUs)
	machineType := fmt.Sprintf("zones/%s/machineTypes/n2-standard-%d", sm.gcpZone, builderVCPUs)
	sourceImage := fmt.Sprintf("projects/%s/global/images/family/%s", sm.gcpProject, "capsule-host")
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
					Email:  proto.String(sm.builderServiceAccountEmail()),
					Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
				},
			},
			Scheduling: &computepb.Scheduling{
				// Refresh builds are fast and cheap to retry — use spot instances.
				// Init builds are long-running — use on-demand instances.
				Preemptible: proto.Bool(buildType == "refresh" || buildType == "reattach"),
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

// GCTerminatedBuilderVMs deletes GCE instances matching "layer-builder-*" that
// are in TERMINATED state. This catches VMs that were partially created but
// never cleaned up (e.g. launch failed after Insert but before status='running').
func (sm *SnapshotManager) GCTerminatedBuilderVMs(ctx context.Context) {
	if sm.gcpProject == "" {
		return
	}

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		sm.logger.WithError(err).Warn("GC: failed to create compute client")
		return
	}
	defer instancesClient.Close()

	it := instancesClient.List(ctx, &computepb.ListInstancesRequest{
		Project: sm.gcpProject,
		Zone:    sm.gcpZone,
		Filter:  proto.String(`name = "layer-builder-*" AND status = "TERMINATED"`),
	})

	var deleted int
	for {
		instance, err := it.Next()
		if err != nil {
			break
		}
		name := instance.GetName()
		sm.logger.WithField("instance", name).Info("GC: deleting terminated builder VM")
		op, err := instancesClient.Delete(ctx, &computepb.DeleteInstanceRequest{
			Project:  sm.gcpProject,
			Zone:     sm.gcpZone,
			Instance: name,
		})
		if err != nil {
			sm.logger.WithError(err).WithField("instance", name).Warn("GC: failed to delete VM")
			continue
		}
		if err := op.Wait(ctx); err != nil {
			sm.logger.WithError(err).WithField("instance", name).Warn("GC: failed waiting for deletion")
			continue
		}
		deleted++
	}
	if deleted > 0 {
		sm.logger.WithField("count", deleted).Info("GC: cleaned up terminated builder VMs")
	}
}

// SnapshotToProto converts a Snapshot to its proto representation
func (sm *SnapshotManager) SnapshotToProto(s *Snapshot) *pb.Snapshot {
	if s == nil {
		return nil
	}
	return &pb.Snapshot{
		Version:    s.Version,
		Status:     s.Status,
		GcsPath:    s.GCSPath,
		RepoCommit: s.RepoCommit,
		SizeBytes:  s.SizeBytes,
		CreatedAt:  timestamppb.New(s.CreatedAt),
	}
}

// --- Repo-scoped methods (Phase 1.6) ---

// GetCurrentSnapshotForKey returns the current active snapshot for a specific repo.
func (sm *SnapshotManager) GetCurrentSnapshotForKey(ctx context.Context, workloadKey string) (*Snapshot, error) {
	var s Snapshot
	var metricsJSON sql.NullString

	err := sm.db.QueryRowContext(ctx, `
		SELECT version, status, gcs_path, repo_commit, size_bytes, created_at, metrics
		FROM snapshots
		WHERE workload_key = $1 AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`, workloadKey).Scan(&s.Version, &s.Status, &s.GCSPath, &s.RepoCommit,
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
		SELECT version, status, gcs_path, repo_commit, size_bytes, created_at, metrics
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

		err := rows.Scan(&s.Version, &s.Status, &s.GCSPath, &s.RepoCommit,
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

// monitorSnapshotBuildForKey monitors a repo-scoped snapshot build.
// After build completes, it auto-validates and optionally auto-rolls out.
//
//nolint:unused // will be wired up when per-key snapshot builds are enabled
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

				// Check if auto-rollout is enabled for this workload key.
				var autoRollout bool
				err := sm.db.QueryRowContext(ctx, `SELECT auto_rollout FROM layered_configs WHERE leaf_workload_key = $1`, workloadKey).Scan(&autoRollout)
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

// GetTargetVersionsByWorkloadKey returns the fleet-wide target version for a
// workload key and any per-host overrides from version_assignments. This
// allows batch resolution of per-host target versions for scoring without
// N+1 queries.
func (sm *SnapshotManager) GetTargetVersionsByWorkloadKey(ctx context.Context, workloadKey string) (fleetVersion string, hostOverrides map[string]string) {
	hostOverrides = make(map[string]string)
	if sm.db == nil || workloadKey == "" {
		return "", hostOverrides
	}

	// Fleet-wide default (host_id IS NULL)
	_ = sm.db.QueryRowContext(ctx, `
		SELECT version FROM version_assignments
		WHERE workload_key = $1 AND host_id IS NULL
	`, workloadKey).Scan(&fleetVersion)

	// Per-host overrides
	rows, err := sm.db.QueryContext(ctx, `
		SELECT host_id, version FROM version_assignments
		WHERE workload_key = $1 AND host_id IS NOT NULL
	`, workloadKey)
	if err != nil {
		return fleetVersion, hostOverrides
	}
	defer rows.Close()

	for rows.Next() {
		var hostID, version string
		if rows.Scan(&hostID, &version) == nil {
			hostOverrides[hostID] = version
		}
	}

	return fleetVersion, hostOverrides
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
