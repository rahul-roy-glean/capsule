package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/tiers"
)

// LayerBuildScheduler manages the build queue for layered snapshot builds.
type LayerBuildScheduler struct {
	db              *sql.DB
	snapshotManager *SnapshotManager
	logger          *logrus.Entry
	maxConcurrent   int
}

// NewLayerBuildScheduler creates a new LayerBuildScheduler.
func NewLayerBuildScheduler(db *sql.DB, sm *SnapshotManager, logger *logrus.Logger, maxConcurrent int) *LayerBuildScheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	return &LayerBuildScheduler{
		db:              db,
		snapshotManager: sm,
		logger:          logger.WithField("component", "layer-builder"),
		maxConcurrent:   maxConcurrent,
	}
}

// Run is the main scheduler loop. It ticks every 10 seconds.
func (s *LayerBuildScheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	gcTicker := time.NewTicker(5 * time.Minute)
	defer gcTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gcTicker.C:
			s.snapshotManager.GCTerminatedBuilderVMs(ctx)
		case <-ticker.C:
			s.processWaitingBuilds(ctx)
			s.processQueuedBuilds(ctx)
			s.checkRefreshSchedules(ctx)
			s.checkRunningBuilds(ctx)
		}
	}
}

// EnqueueChainBuild enqueues builds for a layer chain starting from startDepth.
// configID is stored on each build row so processQueuedBuilds can resolve
// tier and credentials with a simple JOIN instead of a recursive CTE.
func (s *LayerBuildScheduler) EnqueueChainBuild(ctx context.Context, layers []snapshot.LayerMaterialized, startDepth int, buildType string, configID string, force ...bool) error {
	isForce := len(force) > 0 && force[0]

	for i := startDepth; i < len(layers); i++ {
		layer := layers[i]
		now := time.Now()
		version := fmt.Sprintf("v%s-%s-%s", now.Format("20060102-150405"), layer.LayerHash[:8], fmt.Sprintf("%04d", now.Nanosecond()/1e5))

		if isForce {
			// Force: cancel all existing builds so we can enqueue fresh ones.
			s.db.ExecContext(ctx,
				`UPDATE snapshot_builds SET status='cancelled' WHERE layer_hash=$1 AND status IN ('queued','waiting_parent','running')`,
				layer.LayerHash)
		}

		// The partial unique index idx_builds_one_active_per_layer enforces
		// at most one active build per layer at the DB level. The SELECT check
		// here is a fast-path to avoid generating a version string + INSERT
		// attempt that would conflict.
		var existingBuild string
		err := s.db.QueryRowContext(ctx,
			`SELECT build_id FROM snapshot_builds WHERE layer_hash=$1 AND status IN ('queued','waiting_parent','running')`,
			layer.LayerHash).Scan(&existingBuild)
		if err == nil {
			s.logger.WithFields(logrus.Fields{
				"layer_hash": layer.LayerHash[:16],
				"build_id":   existingBuild,
			}).Debug("Layer already has an active build, skipping")
			continue
		}

		// For init builds (non-force), skip layers that already have a current_version.
		// Force and refresh builds always rebuild.
		if buildType == "init" && !isForce {
			var currentVersion sql.NullString
			s.db.QueryRowContext(ctx,
				`SELECT current_version FROM snapshot_layers WHERE layer_hash=$1`,
				layer.LayerHash).Scan(&currentVersion)
			if currentVersion.Valid && currentVersion.String != "" {
				s.logger.WithFields(logrus.Fields{
					"layer_hash":      layer.LayerHash[:16],
					"current_version": currentVersion.String,
					"depth":           layer.Depth,
				}).Info("Layer already has active version, skipping")
				continue
			}
		}

		// Determine parent version
		parentVersion := ""
		if layer.ParentLayerHash != "" {
			s.db.QueryRowContext(ctx,
				`SELECT current_version FROM snapshot_layers WHERE layer_hash=$1`,
				layer.ParentLayerHash).Scan(&parentVersion)
		}

		status := "queued"
		// For force builds, children must wait for parent to finish rebuilding first.
		if isForce && i > startDepth {
			status = "waiting_parent"
		} else if i > startDepth && parentVersion == "" {
			status = "waiting_parent"
		}

		effectiveBuildType := buildType
		var oldLayerHash, oldLayerVersion string
		if i > startDepth {
			// Force rebuild: children refresh from their own current version.
			// This preserves extension drive data (caches, repos) while re-running refresh commands.
			if isForce && hasRefreshCommands(layer) {
				var selfVersion sql.NullString
				s.db.QueryRowContext(ctx,
					`SELECT current_version FROM snapshot_layers WHERE layer_hash=$1`,
					layer.LayerHash).Scan(&selfVersion)
				if selfVersion.Valid && selfVersion.String != "" {
					effectiveBuildType = "refresh"
					oldLayerHash = layer.LayerHash
					oldLayerVersion = selfVersion.String
				} else {
					effectiveBuildType = "init"
				}
			} else {
				// Look for an old layer with same commands, drives, AND parent
				// so we only reattach drives that were built on the same ancestor chain.
				var oldHash, oldVer sql.NullString
				s.db.QueryRowContext(ctx, `
					SELECT sl_old.layer_hash, sl_old.current_version FROM snapshot_layers sl_old
					JOIN snapshot_layers sl_new ON
					  sl_old.init_commands = sl_new.init_commands
					  AND sl_old.drives = sl_new.drives
					  AND sl_old.parent_layer_hash = sl_new.parent_layer_hash
					WHERE sl_new.layer_hash = $1
					  AND sl_old.layer_hash != $1
					  AND sl_old.current_version IS NOT NULL
					ORDER BY sl_old.updated_at DESC LIMIT 1
				`, layer.LayerHash).Scan(&oldHash, &oldVer)

				if oldHash.Valid && hasRefreshCommands(layer) {
					effectiveBuildType = "reattach"
					oldLayerHash = oldHash.String
					oldLayerVersion = oldVer.String
				} else {
					effectiveBuildType = "init"
				}
			}
		}

		_, err = s.db.ExecContext(ctx, `
			INSERT INTO snapshot_builds (layer_hash, version, status, build_type, parent_version, old_layer_hash, old_layer_version, config_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (layer_hash, version) DO NOTHING
		`, layer.LayerHash, version, status, effectiveBuildType, parentVersion,
			sql.NullString{String: oldLayerHash, Valid: oldLayerHash != ""},
			sql.NullString{String: oldLayerVersion, Valid: oldLayerVersion != ""},
			configID)
		if err != nil {
			return fmt.Errorf("failed to enqueue build for layer %s: %w", layer.Name, err)
		}

		s.logger.WithFields(logrus.Fields{
			"layer_hash": layer.LayerHash[:16],
			"version":    version,
			"status":     status,
			"build_type": effectiveBuildType,
			"depth":      layer.Depth,
		}).Info("Enqueued layer build")
	}
	return nil
}

// processWaitingBuilds transitions builds from waiting_parent to queued when parent is ready.
// Uses a single UPDATE ... FROM to avoid N+1 queries.
func (s *LayerBuildScheduler) processWaitingBuilds(ctx context.Context) {
	// Fast path: skip expensive JOINs when nothing is waiting.
	var waitingCount int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshot_builds WHERE status='waiting_parent'`).Scan(&waitingCount)
	if waitingCount == 0 {
		return
	}

	// Unblock root layers that shouldn't be waiting (no parent)
	s.db.ExecContext(ctx, `
		UPDATE snapshot_builds SET status='queued'
		WHERE status = 'waiting_parent'
		  AND layer_hash IN (
		    SELECT sl.layer_hash FROM snapshot_layers sl
		    WHERE sl.parent_layer_hash IS NULL
		  )
	`)

	// Unblock builds whose parent layer has a current_version AND no active build in progress.
	// If the parent has a queued/running build, children keep waiting for it to complete.
	result, err := s.db.ExecContext(ctx, `
		UPDATE snapshot_builds sb SET
			status = 'queued',
			parent_version = parent_sl.current_version
		FROM snapshot_layers sl
		JOIN snapshot_layers parent_sl ON sl.parent_layer_hash = parent_sl.layer_hash
		WHERE sb.status = 'waiting_parent'
		  AND sb.layer_hash = sl.layer_hash
		  AND sl.status = 'active'
		  AND parent_sl.current_version IS NOT NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM snapshot_builds psb
		    WHERE psb.layer_hash = parent_sl.layer_hash
		      AND psb.status IN ('queued', 'waiting_parent', 'running')
		  )
	`)
	if err != nil {
		s.logger.WithError(err).Error("Failed to process waiting builds")
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		s.logger.WithField("unblocked", n).Info("Builds unblocked by parent completion")
	}
}

// processQueuedBuilds atomically claims oldest queued builds and launches VMs.
// Uses FOR UPDATE SKIP LOCKED to prevent duplicate claims across scheduler instances.
// Config tier and credentials are resolved via config_id stored at enqueue time.
func (s *LayerBuildScheduler) processQueuedBuilds(ctx context.Context) {
	var running int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshot_builds WHERE status='running'`).Scan(&running)
	if running >= s.maxConcurrent {
		return
	}

	// Atomically claim queued builds and fetch all needed context in one query.
	// FOR UPDATE SKIP LOCKED prevents concurrent schedulers from claiming the same rows.
	rows, err := s.db.QueryContext(ctx, `
		WITH claimed AS (
			SELECT build_id FROM snapshot_builds
			WHERE status = 'queued'
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		SELECT sb.build_id, sb.layer_hash, sb.version, sb.build_type, sb.parent_version,
		       sl.parent_layer_hash, sl.init_commands, sl.refresh_commands, sl.drives,
		       COALESCE(sl.all_chain_drives, sl.drives) AS all_chain_drives,
		       sl.config_name,
		       parent_sl.current_version AS parent_current_version,
		       lc.tier, lc.github_app_id, lc.github_app_secret,
		       sb.old_layer_hash, sb.old_layer_version
		FROM snapshot_builds sb
		JOIN claimed c ON sb.build_id = c.build_id
		JOIN snapshot_layers sl ON sb.layer_hash = sl.layer_hash
		LEFT JOIN snapshot_layers parent_sl ON sl.parent_layer_hash = parent_sl.layer_hash
		LEFT JOIN layered_configs lc ON lc.config_id = sb.config_id
		WHERE sl.status != 'inactive'
	`, s.maxConcurrent-running)
	if err != nil {
		s.logger.WithError(err).Error("Failed to query queued builds")
		return
	}
	defer rows.Close()

	type buildRow struct {
		buildID              string
		layerHash            string
		version              string
		buildType            string
		parentVersion        sql.NullString
		parentLayerHash      sql.NullString
		initCmdsJSON         string
		refreshCmdsJSON      string
		drivesJSON           string
		allChainDrivesJSON   string
		configName           string
		parentCurrentVersion sql.NullString
		tier                 sql.NullString
		githubAppID          sql.NullString
		githubAppSecret      sql.NullString
		oldLayerHash         string
		oldLayerVersion      string
	}

	var builds []buildRow
	for rows.Next() {
		var b buildRow
		var oldHash, oldVer sql.NullString
		if err := rows.Scan(&b.buildID, &b.layerHash, &b.version, &b.buildType, &b.parentVersion,
			&b.parentLayerHash, &b.initCmdsJSON, &b.refreshCmdsJSON, &b.drivesJSON, &b.allChainDrivesJSON,
			&b.configName,
			&b.parentCurrentVersion, &b.tier, &b.githubAppID, &b.githubAppSecret,
			&oldHash, &oldVer); err != nil {
			continue
		}
		if oldHash.Valid {
			b.oldLayerHash = oldHash.String
		}
		if oldVer.Valid {
			b.oldLayerVersion = oldVer.String
		}
		builds = append(builds, b)
	}

	for _, b := range builds {
		// Verify parent is ready for non-root layers
		if b.parentLayerHash.Valid && b.parentLayerHash.String != "" {
			if !b.parentCurrentVersion.Valid || b.parentCurrentVersion.String == "" {
				s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='waiting_parent' WHERE build_id=$1`, b.buildID)
				continue
			}
			// Use the resolved parent version from the JOIN
			b.parentVersion = b.parentCurrentVersion
		}

		// Choose commands based on build type
		commandsJSON := b.initCmdsJSON
		if (b.buildType == "refresh" || b.buildType == "reattach") && b.refreshCmdsJSON != "" && b.refreshCmdsJSON != "[]" && b.refreshCmdsJSON != "null" {
			commandsJSON = b.refreshCmdsJSON
		}

		// Launch build VM
		instanceName := fmt.Sprintf("layer-builder-%s-%s", b.layerHash[:8], b.version)
		parentWorkloadKey := ""
		parentVersion := ""
		if b.parentLayerHash.Valid {
			parentWorkloadKey = b.parentLayerHash.String
		}
		if b.parentVersion.Valid {
			parentVersion = b.parentVersion.String
		}

		// Resolve tier from the joined config (already fetched, no extra query)
		snapshotVCPUs := 4
		snapshotMemoryMB := 4096
		if b.tier.Valid && b.tier.String != "" {
			if t, err := tiers.Lookup(b.tier.String); err == nil {
				snapshotVCPUs = t.VCPUs
				snapshotMemoryMB = t.MemoryMB
			}
		}

		githubAppID := ""
		githubAppSecret := ""
		if b.githubAppID.Valid {
			githubAppID = b.githubAppID.String
		}
		if b.githubAppSecret.Valid {
			githubAppSecret = b.githubAppSecret.String
		}

		// Extract base-image and runner-user from init commands
		baseImage := ""
		runnerUser := ""
		var initCmds []snapshot.SnapshotCommand
		json.Unmarshal([]byte(b.initCmdsJSON), &initCmds)
		for _, cmd := range initCmds {
			if cmd.Type == "base-image" && len(cmd.Args) > 0 {
				baseImage = cmd.Args[0]
			}
			if cmd.Type == "platform-user" && len(cmd.Args) > 0 {
				runnerUser = cmd.Args[0]
			}
		}

		err := s.launchLayerBuildVM(ctx, instanceName, b.layerHash, commandsJSON, b.version,
			parentWorkloadKey, parentVersion, b.allChainDrivesJSON, b.buildType,
			githubAppID, githubAppSecret, snapshotVCPUs, snapshotMemoryMB,
			baseImage, runnerUser, b.oldLayerHash, b.oldLayerVersion)
		if err != nil {
			s.logger.WithError(err).WithField("build_id", b.buildID).Error("Failed to launch layer build VM")
			s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='failed', failure_reason=$2 WHERE build_id=$1`, b.buildID, err.Error())
			// Clean up VM if it was partially created before the error
			s.snapshotManager.cleanupBuilderVM(ctx, instanceName)
			continue
		}

		s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='running', started_at=NOW(), instance_name=$2 WHERE build_id=$1`, b.buildID, instanceName)
		s.logger.WithFields(logrus.Fields{
			"build_id":   b.buildID,
			"layer_hash": b.layerHash[:16],
			"instance":   instanceName,
		}).Info("Layer build VM launched")
	}
}

// checkRunningBuilds monitors running builds for timeout or VM termination.
func (s *LayerBuildScheduler) checkRunningBuilds(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT build_id, layer_hash, version, instance_name, started_at, retry_count, max_retries
		FROM snapshot_builds WHERE status='running'
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	type runningBuild struct {
		buildID      string
		layerHash    string
		version      string
		instanceName sql.NullString
		startedAt    time.Time
		retryCount   int
		maxRetries   int
	}

	var builds []runningBuild
	for rows.Next() {
		var b runningBuild
		if err := rows.Scan(&b.buildID, &b.layerHash, &b.version, &b.instanceName, &b.startedAt, &b.retryCount, &b.maxRetries); err != nil {
			continue
		}
		builds = append(builds, b)
	}

	// Check builds in parallel since each may hit GCS + GCE APIs
	type buildResult struct {
		build     runningBuild
		completed bool
		timedOut  bool
		vmGone    bool
	}
	results := make([]buildResult, len(builds))
	var wg sync.WaitGroup

	for i, b := range builds {
		results[i].build = b

		if time.Since(b.startedAt) > 45*time.Minute {
			results[i].timedOut = true
			continue
		}

		wg.Add(1)
		go func(idx int, b runningBuild) {
			defer wg.Done()
			// Check GCS for completion marker
			complete, err := s.snapshotManager.checkLayerBuildComplete(ctx, b.layerHash, b.version)
			if err == nil && complete {
				results[idx].completed = true
				return
			}
			// Check if VM is still running
			if b.instanceName.Valid && s.snapshotManager.gcpProject != "" {
				running, err := s.snapshotManager.isBuilderVMRunning(ctx, b.instanceName.String)
				if err == nil && !running {
					results[idx].vmGone = true
					// Final GCS check after VM termination
					complete, _ := s.snapshotManager.checkLayerBuildComplete(ctx, b.layerHash, b.version)
					results[idx].completed = complete
				}
			}
		}(i, b)
	}
	wg.Wait()

	// Process results sequentially (DB writes + cascading logic are not concurrent-safe)
	for _, r := range results {
		b := r.build
		if r.timedOut {
			s.logger.WithFields(logrus.Fields{
				"build_id":   b.buildID,
				"layer_hash": b.layerHash[:16],
			}).Error("Layer build timed out")
			s.onBuildFailed(ctx, b.buildID, b.layerHash, "build timed out", b.retryCount, b.maxRetries)
			if b.instanceName.Valid {
				s.snapshotManager.cleanupBuilderVM(ctx, b.instanceName.String)
			}
			continue
		}
		if r.completed {
			s.onBuildComplete(ctx, b.buildID, b.layerHash, b.version)
			if b.instanceName.Valid {
				s.snapshotManager.cleanupBuilderVM(ctx, b.instanceName.String)
			}
			continue
		}
		if r.vmGone {
			s.onBuildFailed(ctx, b.buildID, b.layerHash, "VM terminated without completing", b.retryCount, b.maxRetries)
			s.snapshotManager.cleanupBuilderVM(ctx, b.instanceName.String)
		}
	}
}

// onBuildComplete handles a successful build completion.
func (s *LayerBuildScheduler) onBuildComplete(ctx context.Context, buildID, layerHash, version string) {
	s.logger.WithFields(logrus.Fields{
		"build_id":   buildID,
		"layer_hash": layerHash[:16],
		"version":    version,
	}).Info("Layer build completed")

	// For leaf layers, the GCS alias must succeed before we mark the layer
	// as active — otherwise hosts won't find the snapshot under the workload key.
	var autoRollout bool
	var leafWorkloadKey string
	isLeaf := false
	err := s.db.QueryRowContext(ctx, `SELECT leaf_workload_key, auto_rollout FROM layered_configs WHERE leaf_layer_hash=$1`, layerHash).Scan(&leafWorkloadKey, &autoRollout)
	if err == nil {
		isLeaf = true
		if leafErr := s.onLeafLayerComplete(ctx, layerHash, version, leafWorkloadKey, autoRollout); leafErr != nil {
			s.logger.WithError(leafErr).Error("Leaf layer GCS alias failed, marking build as failed")
			s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='failed', failure_reason=$2, completed_at=NOW() WHERE build_id=$1`, buildID, leafErr.Error())
			return
		}
	}

	// Update layer status
	s.db.ExecContext(ctx, `UPDATE snapshot_layers SET current_version=$1, status='active', updated_at=NOW() WHERE layer_hash=$2`, version, layerHash)
	s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='completed', completed_at=NOW() WHERE build_id=$1`, buildID)

	// Unblock waiting children (leaf layers have no children, skip the query)
	if !isLeaf {
		rows, err := s.db.QueryContext(ctx, `
			SELECT sb.build_id, sb.layer_hash FROM snapshot_builds sb
			JOIN snapshot_layers sl ON sb.layer_hash = sl.layer_hash
			WHERE sb.status='waiting_parent' AND sl.parent_layer_hash=$1
		`, layerHash)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var childBuildID, childLayerHash string
				if err := rows.Scan(&childBuildID, &childLayerHash); err != nil {
					continue
				}
				s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='queued', parent_version=$2 WHERE build_id=$1`, childBuildID, version)
				s.logger.WithFields(logrus.Fields{
					"child_build_id": childBuildID,
					"parent_version": version,
				}).Info("Child build unblocked")
			}
		}
	}
}

// onLeafLayerComplete handles completion of a leaf layer build.
// Returns an error if the GCS alias fails — callers should not mark the build
// as completed when this returns non-nil.
func (s *LayerBuildScheduler) onLeafLayerComplete(ctx context.Context, layerHash, version, workloadKey string, autoRollout bool) error {
	s.logger.WithFields(logrus.Fields{
		"layer_hash":   layerHash[:16],
		"version":      version,
		"workload_key": workloadKey,
		"auto_rollout": autoRollout,
	}).Info("Leaf layer build completed")

	// Create GCS alias: copy chunked metadata from layer_hash path to workload_key path
	// so the host agent can find it by workload key.
	if err := s.createWorkloadKeyAlias(ctx, layerHash, version, workloadKey); err != nil {
		return fmt.Errorf("failed to create workload key GCS alias: %w", err)
	}

	// Insert into snapshots table for the rollout pipeline
	metricsJSON, _ := json.Marshal(SnapshotMetrics{})
	s.db.ExecContext(ctx, `
		INSERT INTO snapshots (version, status, workload_key, gcs_path, bazel_version, repo_commit, size_bytes, metrics)
		VALUES ($1, 'ready', $2, '', '', '', 0, $3)
		ON CONFLICT (version) DO NOTHING
	`, version, workloadKey, string(metricsJSON))

	if autoRollout {
		s.logger.WithField("version", version).Info("Auto-rollout: setting active snapshot")
		s.snapshotManager.SetActiveSnapshotForKey(ctx, workloadKey, version)
	}

	return nil
}

// createWorkloadKeyAlias copies chunked metadata from the layer hash GCS path
// to the workload key GCS path so host agents can look up snapshots by workload key.
func (s *LayerBuildScheduler) createWorkloadKeyAlias(ctx context.Context, layerHash, version, workloadKey string) error {
	sm := s.snapshotManager
	bucket := sm.gcsClient.Bucket(sm.gcsBucket)
	prefix := sm.gcsPrefix

	// Source: {prefix}/{layer_hash}/snapshot_state/{version}/chunked-metadata.json
	srcMeta := fmt.Sprintf("%s/%s/snapshot_state/%s/chunked-metadata.json", prefix, layerHash, version)
	// Dest: {prefix}/{workload_key}/snapshot_state/{version}/chunked-metadata.json
	dstMeta := fmt.Sprintf("%s/%s/snapshot_state/%s/chunked-metadata.json", prefix, workloadKey, version)

	// Copy chunked-metadata.json
	src := bucket.Object(srcMeta)
	dst := bucket.Object(dstMeta)
	if _, err := dst.CopierFrom(src).Run(ctx); err != nil {
		return fmt.Errorf("failed to copy chunked metadata from %s to %s: %w", srcMeta, dstMeta, err)
	}
	s.logger.WithFields(logrus.Fields{
		"src": srcMeta,
		"dst": dstMeta,
	}).Info("Copied chunked metadata to workload key path")

	// Write current-pointer.json under workload key
	pointerPath := fmt.Sprintf("%s/%s/current-pointer.json", prefix, workloadKey)
	pointerData := fmt.Sprintf(`{"version":"%s"}`, version)
	w := bucket.Object(pointerPath).NewWriter(ctx)
	w.ContentType = "application/json"
	if _, err := w.Write([]byte(pointerData)); err != nil {
		w.Close()
		return fmt.Errorf("failed to write current pointer: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to close current pointer writer: %w", err)
	}
	s.logger.WithField("path", pointerPath).Info("Updated current-pointer.json for workload key")

	return nil
}

// onBuildFailed handles a failed build. Retries if under max retries, otherwise cancels downstream.
func (s *LayerBuildScheduler) onBuildFailed(ctx context.Context, buildID, layerHash, reason string, retryCount, maxRetries int) {
	// Don't retry builds for inactive/orphaned layers
	var layerStatus sql.NullString
	s.db.QueryRowContext(ctx, `SELECT status FROM snapshot_layers WHERE layer_hash=$1`, layerHash).Scan(&layerStatus)
	if layerStatus.Valid && layerStatus.String == "inactive" {
		s.logger.WithFields(logrus.Fields{
			"build_id":   buildID,
			"layer_hash": layerHash[:16],
		}).Info("Layer is inactive, not retrying build")
		s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='cancelled', failure_reason='layer inactive', completed_at=NOW() WHERE build_id=$1`, buildID)
		return
	}

	if retryCount < maxRetries {
		s.logger.WithFields(logrus.Fields{
			"build_id":    buildID,
			"retry_count": retryCount + 1,
			"max_retries": maxRetries,
		}).Warn("Layer build failed, requeueing for retry")
		s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='queued', retry_count=retry_count+1, failure_reason=$2 WHERE build_id=$1`, buildID, reason)
		return
	}

	s.logger.WithFields(logrus.Fields{
		"build_id":   buildID,
		"layer_hash": layerHash[:16],
		"reason":     reason,
	}).Error("Layer build failed permanently")
	s.db.ExecContext(ctx, `UPDATE snapshot_builds SET status='failed', failure_reason=$2, completed_at=NOW() WHERE build_id=$1`, buildID, reason)

	// Cancel all downstream waiting builds
	s.cancelDescendantBuilds(ctx, layerHash, "parent build failed: "+reason)
}

// cancelDescendantBuilds cancels all waiting/queued builds for descendants of the given layer.
// Uses a recursive CTE to walk the entire subtree in a single query.
func (s *LayerBuildScheduler) cancelDescendantBuilds(ctx context.Context, parentLayerHash, reason string) {
	result, err := s.db.ExecContext(ctx, `
		WITH RECURSIVE descendants AS (
			SELECT layer_hash FROM snapshot_layers WHERE parent_layer_hash = $1
			UNION ALL
			SELECT sl.layer_hash FROM snapshot_layers sl
			JOIN descendants d ON sl.parent_layer_hash = d.layer_hash
		)
		UPDATE snapshot_builds SET status='cancelled', failure_reason=$2, completed_at=NOW()
		WHERE layer_hash IN (SELECT layer_hash FROM descendants)
		  AND status IN ('queued','waiting_parent')
	`, parentLayerHash, reason)
	if err != nil {
		s.logger.WithError(err).Error("Failed to cancel descendant builds")
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		s.logger.WithFields(logrus.Fields{
			"parent":    parentLayerHash[:16],
			"cancelled": n,
		}).Info("Cancelled descendant builds")
	}
}

// checkRefreshSchedules checks layers with refresh_interval and enqueues refreshes when due.
// Uses a single query with subqueries to fetch last build time and active build status,
// avoiding per-layer round trips.
func (s *LayerBuildScheduler) checkRefreshSchedules(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sl.layer_hash, sl.refresh_interval, sl.current_version,
		       (SELECT MAX(completed_at) FROM snapshot_builds
		        WHERE layer_hash = sl.layer_hash AND status = 'completed') AS last_completed,
		       EXISTS(SELECT 1 FROM snapshot_builds
		              WHERE layer_hash = sl.layer_hash AND status IN ('queued','waiting_parent','running')) AS has_active_build
		FROM snapshot_layers sl
		WHERE sl.refresh_interval != '' AND sl.refresh_interval != 'on_push' AND sl.status = 'active'
		  AND sl.current_version IS NOT NULL
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var layerHash, refreshInterval string
		var currentVersion sql.NullString
		var lastCompleted sql.NullTime
		var hasActiveBuild bool
		if err := rows.Scan(&layerHash, &refreshInterval, &currentVersion, &lastCompleted, &hasActiveBuild); err != nil {
			continue
		}
		if hasActiveBuild {
			continue
		}

		interval, err := time.ParseDuration(refreshInterval)
		if err != nil {
			continue
		}

		if lastCompleted.Valid && now.Sub(lastCompleted.Time) <= interval {
			continue
		}

		// Look up owning config for this layer (needed for config_id on build rows)
		configID := s.lookupConfigIDForLayer(ctx, layerHash)

		// Enqueue a refresh build for this layer
		version := fmt.Sprintf("v%s-%s-%s", now.Format("20060102-150405"), layerHash[:8], fmt.Sprintf("%04d", now.Nanosecond()/1e5))
		s.db.ExecContext(ctx, `
			INSERT INTO snapshot_builds (layer_hash, version, status, build_type, config_id)
			VALUES ($1, $2, 'queued', 'refresh', $3)
			ON CONFLICT (layer_hash, version) DO NOTHING
		`, layerHash, version, configID)

		s.logger.WithFields(logrus.Fields{
			"layer_hash": layerHash[:16],
			"interval":   refreshInterval,
		}).Info("Refresh schedule triggered")

		// Cascade: enqueue init builds for all children
		s.enqueueChildRebuilds(ctx, layerHash, configID)
	}
}

// enqueueChildRebuilds enqueues init builds for all descendant layers.
// Uses a recursive CTE to find all descendants, then inserts builds for those
// that don't already have an active build.
func (s *LayerBuildScheduler) enqueueChildRebuilds(ctx context.Context, parentLayerHash string, configID string) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE descendants AS (
			SELECT layer_hash, parent_layer_hash, init_commands, drives, refresh_commands FROM snapshot_layers WHERE parent_layer_hash = $1
			UNION ALL
			SELECT sl.layer_hash, sl.parent_layer_hash, sl.init_commands, sl.drives, sl.refresh_commands FROM snapshot_layers sl
			JOIN descendants d ON sl.parent_layer_hash = d.layer_hash
		)
		SELECT d.layer_hash, d.parent_layer_hash, d.refresh_commands FROM descendants d
		WHERE NOT EXISTS (
			SELECT 1 FROM snapshot_builds sb
			WHERE sb.layer_hash = d.layer_hash
			  AND sb.status IN ('queued','waiting_parent','running')
		)
	`, parentLayerHash)
	if err != nil {
		s.logger.WithError(err).Error("Failed to query descendant layers")
		return
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var childHash string
		var childParentHash sql.NullString
		var refreshCmdsJSON sql.NullString
		if err := rows.Scan(&childHash, &childParentHash, &refreshCmdsJSON); err != nil {
			continue
		}
		version := fmt.Sprintf("v%s-%s-%s", now.Format("20060102-150405"), childHash[:8], fmt.Sprintf("%04d", now.Nanosecond()/1e5))

		buildType := "init"
		var oldLayerHash, oldLayerVersion string
		var oldHash, oldVer sql.NullString
		// Match on parent_layer_hash to ensure the old layer shares the same ancestor chain
		s.db.QueryRowContext(ctx, `
			SELECT layer_hash, current_version FROM snapshot_layers
			WHERE init_commands = (SELECT init_commands FROM snapshot_layers WHERE layer_hash = $1)
			  AND drives = (SELECT drives FROM snapshot_layers WHERE layer_hash = $1)
			  AND parent_layer_hash = (SELECT parent_layer_hash FROM snapshot_layers WHERE layer_hash = $1)
			  AND layer_hash != $1 AND current_version IS NOT NULL
			ORDER BY updated_at DESC LIMIT 1
		`, childHash).Scan(&oldHash, &oldVer)

		hasRefresh := refreshCmdsJSON.Valid && refreshCmdsJSON.String != "" && refreshCmdsJSON.String != "[]" && refreshCmdsJSON.String != "null"
		if oldHash.Valid && hasRefresh {
			buildType = "reattach"
			oldLayerHash = oldHash.String
			oldLayerVersion = oldVer.String
		}

		s.db.ExecContext(ctx, `
			INSERT INTO snapshot_builds (layer_hash, version, status, build_type, old_layer_hash, old_layer_version, config_id)
			VALUES ($1, $2, 'waiting_parent', $3, $4, $5, $6)
			ON CONFLICT (layer_hash, version) DO NOTHING
		`, childHash, version, buildType,
			sql.NullString{String: oldLayerHash, Valid: oldLayerHash != ""},
			sql.NullString{String: oldLayerVersion, Valid: oldLayerVersion != ""},
			configID)
	}
}

// RebuildFromLayer triggers a rebuild of a specific layer and all its descendants.
func (s *LayerBuildScheduler) RebuildFromLayer(ctx context.Context, layerHash string) error {
	configID := s.lookupConfigIDForLayer(ctx, layerHash)

	// Enqueue build for this layer
	now := time.Now()
	version := fmt.Sprintf("v%s-%s-%s", now.Format("20060102-150405"), layerHash[:8], fmt.Sprintf("%04d", now.Nanosecond()/1e5))

	var activeBuild string
	err := s.db.QueryRowContext(ctx,
		`SELECT build_id FROM snapshot_builds WHERE layer_hash=$1 AND status IN ('queued','waiting_parent','running')`,
		layerHash).Scan(&activeBuild)
	if err != nil {
		// No active build, enqueue one
		s.db.ExecContext(ctx, `
			INSERT INTO snapshot_builds (layer_hash, version, status, build_type, config_id)
			VALUES ($1, $2, 'queued', 'init', $3)
			ON CONFLICT (layer_hash, version) DO NOTHING
		`, layerHash, version, configID)
	}

	// Enqueue rebuilds for all descendants
	s.enqueueChildRebuilds(ctx, layerHash, configID)

	return nil
}

// lookupConfigIDForLayer walks descendants to find which config owns a layer.
// This is used by background processes (refresh schedules, manual rebuilds)
// that don't have config context from an HTTP request.
func (s *LayerBuildScheduler) lookupConfigIDForLayer(ctx context.Context, layerHash string) string {
	var configID string
	s.db.QueryRowContext(ctx, `
		WITH RECURSIVE descendants AS (
			SELECT layer_hash FROM snapshot_layers WHERE layer_hash = $1
			UNION ALL
			SELECT sl.layer_hash FROM snapshot_layers sl
			JOIN descendants d ON sl.parent_layer_hash = d.layer_hash
		)
		SELECT lc.config_id FROM layered_configs lc
		WHERE lc.leaf_layer_hash IN (SELECT layer_hash FROM descendants)
		LIMIT 1
	`, layerHash).Scan(&configID)
	return configID
}

// GCOrphanedLayers cleans up layers not referenced by any layered_configs.
// Uses a recursive CTE to find all layers reachable from any config's leaf,
// then deletes unreachable layers older than 7 days.
func (s *LayerBuildScheduler) GCOrphanedLayers(ctx context.Context) {
	// Find layers that ARE referenced: walk up from every leaf_layer_hash
	// to the root. Everything not in this set is orphaned.
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE reachable AS (
			-- Start from all leaf layers referenced by configs
			SELECT sl.layer_hash FROM snapshot_layers sl
			JOIN layered_configs lc ON lc.leaf_layer_hash = sl.layer_hash
			UNION ALL
			-- Walk up to ancestors
			SELECT parent.layer_hash FROM snapshot_layers parent
			JOIN reachable r ON parent.layer_hash = (
				SELECT sl2.parent_layer_hash FROM snapshot_layers sl2
				WHERE sl2.layer_hash = r.layer_hash AND sl2.parent_layer_hash IS NOT NULL
			)
		)
		SELECT sl.layer_hash FROM snapshot_layers sl
		WHERE sl.layer_hash NOT IN (SELECT layer_hash FROM reachable)
		  AND sl.layer_hash NOT IN (
		    SELECT old_layer_hash FROM snapshot_builds
		    WHERE old_layer_hash IS NOT NULL AND status IN ('queued','waiting_parent','running')
		  )
		  AND sl.updated_at < NOW() - INTERVAL '7 days'
	`)
	if err != nil {
		s.logger.WithError(err).Error("Failed to query orphaned layers")
		return
	}
	defer rows.Close()

	var orphaned []string
	for rows.Next() {
		var layerHash string
		if err := rows.Scan(&layerHash); err != nil {
			continue
		}
		orphaned = append(orphaned, layerHash)
	}

	// Delete in reverse depth order (children first) to respect FK constraints
	for i := len(orphaned) - 1; i >= 0; i-- {
		h := orphaned[i]
		s.db.ExecContext(ctx, `DELETE FROM snapshot_builds WHERE layer_hash=$1`, h)
		s.db.ExecContext(ctx, `DELETE FROM snapshot_layers WHERE layer_hash=$1`, h)
		if len(h) >= 16 {
			s.logger.WithField("layer_hash", h[:16]).Info("GC'd orphaned layer")
		}
	}
}

// hasRefreshCommands returns true if the layer has non-empty refresh commands.
func hasRefreshCommands(layer snapshot.LayerMaterialized) bool {
	return len(layer.RefreshCommands) > 0
}

// launchLayerBuildVM creates a GCE instance to build a layer snapshot.
// It builds its own startup script with all layer-specific flags instead of
// delegating to launchSnapshotBuilderVMForKey.
func (s *LayerBuildScheduler) launchLayerBuildVM(ctx context.Context, instanceName, layerHash, commandsJSON, version, parentWorkloadKey, parentVersion, drivesJSON, buildType, githubAppID, githubAppSecret string, snapshotVCPUs, snapshotMemoryMB int, baseImage, runnerUser, oldLayerHash, oldLayerVersion string) error {
	if s.snapshotManager.gcpProject == "" {
		s.logger.Warn("GCP project not configured, skipping VM launch")
		return nil
	}

	s.logger.WithFields(logrus.Fields{
		"instance":   instanceName,
		"layer_hash": layerHash[:16],
		"version":    version,
		"build_type": buildType,
	}).Info("Launching layer build VM")

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create compute client: %w", err)
	}
	defer instancesClient.Close()

	sm := s.snapshotManager
	gcsBase := sm.gcsPath("build-artifacts")

	// Build optional flags
	githubFlags := ""
	if githubAppID != "" && githubAppSecret != "" {
		githubFlags = fmt.Sprintf(`--github-app-id="%s" --github-app-secret="%s" --gcp-project="%s"`,
			githubAppID, githubAppSecret, sm.gcpProject)
	}

	// Layer-specific flags
	layerFlags := fmt.Sprintf(`--layer-hash="%s" --layer-drives='%s' --build-type="%s"`,
		layerHash, drivesJSON, buildType)
	if parentWorkloadKey != "" {
		layerFlags += fmt.Sprintf(` --parent-workload-key="%s"`, parentWorkloadKey)
	}
	if parentVersion != "" {
		layerFlags += fmt.Sprintf(` --parent-version="%s"`, parentVersion)
	}
	if baseImage != "" {
		layerFlags += fmt.Sprintf(` --base-image="%s"`, baseImage)
	}
	if runnerUser != "" && runnerUser != "runner" {
		layerFlags += fmt.Sprintf(` --runner-user="%s"`, runnerUser)
	}
	if oldLayerHash != "" {
		layerFlags += fmt.Sprintf(` --previous-layer-key="%s"`, oldLayerHash)
	}
	if oldLayerVersion != "" {
		layerFlags += fmt.Sprintf(` --previous-layer-version="%s"`, oldLayerVersion)
	}

	startupScript := fmt.Sprintf(`#!/bin/bash
set -e
exec > >(tee /var/log/snapshot-builder.log) 2>&1
# Ensure VM shuts down on failure so it doesn't block the build queue
trap 'echo "Build failed, shutting down..."; shutdown -h now' ERR
echo "Starting layer build setup..."

# Start Docker (pre-installed in Packer image)
systemctl start docker

# Authenticate Docker to Artifact Registry
gcloud auth configure-docker us-central1-docker.pkg.dev --quiet 2>/dev/null || true

# Download kernel and rootfs from GCS (only for init builds without a parent)
mkdir -p /opt/firecracker
gcloud storage cp "gs://%s/%s/kernel.bin" /opt/firecracker/kernel.bin 2>/dev/null \
    || echo "INFO: kernel.bin not in GCS, will use bundled"
gcloud storage cp "gs://%s/%s/%s/rootfs.img" /opt/firecracker/rootfs.img 2>/dev/null \
    || gcloud storage cp "gs://%s/%s/rootfs.img" /opt/firecracker/rootfs.img 2>/dev/null \
    || echo "INFO: rootfs.img not in GCS (expected for child/reattach layers)"

# Download snapshot-builder binary (always download fresh to pick up new deploys)
gcloud storage cp gs://%s/%s/snapshot-builder /usr/local/bin/snapshot-builder
chmod +x /usr/local/bin/snapshot-builder

# Download thaw-agent binary (needed for platform shim injection)
gcloud storage cp gs://%s/%s/thaw-agent /usr/local/bin/thaw-agent
chmod +x /usr/local/bin/thaw-agent

# Decode snapshot commands from base64 to avoid shell quoting issues
SNAPSHOT_COMMANDS=$(echo '%s' | base64 -d)

# Run snapshot builder
/usr/local/bin/snapshot-builder \
    --snapshot-commands="$SNAPSHOT_COMMANDS" \
    --gcs-bucket="%s" \
    --gcs-prefix="%s" \
    --output-dir=/tmp/snapshot \
    --log-level=info \
    --vcpus=%d \
    --memory-mb=%d \
    --version="%s" \
    %s %s
echo "Layer build complete, shutting down..."
shutdown -h now
`, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, layerHash, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, base64.StdEncoding.EncodeToString([]byte(commandsJSON)), sm.gcsBucket, sm.gcsPrefix, snapshotVCPUs, snapshotMemoryMB, version, githubFlags, layerFlags)

	// Size the builder VM
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
			Name:         proto.String(instanceName),
			MachineType:  proto.String(machineType),
			CanIpForward: proto.Bool(true),
			Disks: []*computepb.AttachedDisk{
				{
					Boot:       proto.Bool(true),
					AutoDelete: proto.Bool(true),
					InitializeParams: &computepb.AttachedDiskInitializeParams{
						SourceImage: proto.String(sourceImage),
						DiskSizeGb:  proto.Int64(200),
						DiskType:    proto.String(fmt.Sprintf("zones/%s/diskTypes/pd-ssd", sm.gcpZone)),
					},
				},
			},
			NetworkInterfaces: []*computepb.NetworkInterface{
				{
					Network: proto.String(networkURL),
					AccessConfigs: []*computepb.AccessConfig{
						{
							Type: proto.String("ONE_TO_ONE_NAT"),
							Name: proto.String("External NAT"),
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
				},
			},
			ServiceAccounts: []*computepb.ServiceAccount{
				{
					Email:  proto.String("default"),
					Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
				},
			},
			AdvancedMachineFeatures: &computepb.AdvancedMachineFeatures{
				EnableNestedVirtualization: proto.Bool(true),
			},
			Scheduling: &computepb.Scheduling{
				// Refresh/reattach builds are fast — use spot instances.
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

	s.logger.WithField("instance", instanceName).Info("Layer build VM created")
	return nil
}
