package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
	"github.com/rahul-roy-glean/capsule/pkg/tiers"
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
			s.reconcileCompletedBuilds(ctx)
		case <-ticker.C:
			s.processWaitingBuilds(ctx)
			s.processQueuedBuilds(ctx)
			s.checkRefreshSchedules(ctx)
			s.checkRunningBuilds(ctx)
		}
	}
}

// reconcileCompletedBuilds fixes layers stuck in 'pending' status when their
// builds completed successfully. This can happen if the process was interrupted
// (e.g., pod restart) between marking a build as 'completed' and updating the
// layer status to 'active'. Runs periodically as a safety net.
func (s *LayerBuildScheduler) reconcileCompletedBuilds(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sb.build_id, sb.layer_hash, sb.version, COALESCE(sb.all_chain_drives::text, '[]')
		FROM snapshot_builds sb
		JOIN snapshot_layers sl ON sb.layer_hash = sl.layer_hash
		WHERE sb.status = 'completed'
		  AND sl.status = 'pending'
		  AND (sl.current_version IS NULL OR sl.current_version = '')
		ORDER BY sb.completed_at DESC
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	// Track which layer_hashes we've already fixed (take the latest completed build)
	fixed := make(map[string]bool)
	for rows.Next() {
		var buildID, layerHash, version, buildAllChainDrives string
		if err := rows.Scan(&buildID, &layerHash, &version, &buildAllChainDrives); err != nil {
			continue
		}
		if fixed[layerHash] {
			continue
		}
		fixed[layerHash] = true

		artifactHash := s.snapshotManager.ThawAgentHash(ctx)
		drivesHash := computeAllChainDrivesHash(buildAllChainDrives)
		if _, err := s.db.ExecContext(ctx, `UPDATE snapshot_layers SET current_version=$1, build_artifact_hash=$3, all_chain_drives_hash=$4, status='active', updated_at=NOW() WHERE layer_hash=$2 AND status='pending'`, version, layerHash, artifactHash, drivesHash); err != nil {
			s.logger.WithError(err).WithField("layer_hash", layerHash[:16]).Error("Reconcile: failed to activate layer")
			continue
		}
		s.logger.WithFields(logrus.Fields{
			"build_id":   buildID,
			"layer_hash": layerHash[:16],
			"version":    version,
		}).Warn("Reconcile: activated layer from completed build (was stuck pending)")
	}
}

// isPlatformLayerStale checks whether the capsule-thaw-agent binary in GCS has changed
// since the platform layer was last built. Returns true if the layer should be
// rebuilt (hash mismatch or no stored hash).
func (s *LayerBuildScheduler) isPlatformLayerStale(ctx context.Context, layerHash string) bool {
	currentHash := s.snapshotManager.ThawAgentHash(ctx)
	if currentHash == "" {
		return false // can't check, assume not stale
	}
	var storedHash sql.NullString
	s.db.QueryRowContext(ctx,
		`SELECT build_artifact_hash FROM snapshot_layers WHERE layer_hash=$1`,
		layerHash).Scan(&storedHash)
	if !storedHash.Valid || storedHash.String == "" {
		return true // no stored hash, assume stale
	}
	return storedHash.String != currentHash
}

// computeAllChainDrivesHash returns the SHA256 hex digest of the all_chain_drives
// JSON string. Returns "" for empty, null, or "[]" inputs.
func computeAllChainDrivesHash(allChainDrivesJSON string) string {
	if allChainDrivesJSON == "" || allChainDrivesJSON == "[]" || allChainDrivesJSON == "null" {
		return ""
	}
	h := sha256.Sum256([]byte(allChainDrivesJSON))
	return hex.EncodeToString(h[:])
}

// isAllChainDrivesStale checks whether the all_chain_drives for a given layer+config
// have changed since the layer was last built. Returns true if the stored hash
// does not match the current all_chain_drives from config_layer_settings.
// Returns false when the stored hash is empty (avoids thundering herd on first
// deploy — hash gets populated on next natural build).
func (s *LayerBuildScheduler) isAllChainDrivesStale(ctx context.Context, layerHash, configID string) bool {
	var currentDrivesJSON string
	s.db.QueryRowContext(ctx,
		`SELECT COALESCE(all_chain_drives::text, '[]') FROM config_layer_settings WHERE config_id=$1 AND layer_hash=$2`,
		configID, layerHash).Scan(&currentDrivesJSON)
	currentHash := computeAllChainDrivesHash(currentDrivesJSON)
	if currentHash == "" {
		return false // no drives configured, nothing to detect
	}

	var storedHash sql.NullString
	s.db.QueryRowContext(ctx,
		`SELECT all_chain_drives_hash FROM snapshot_layers WHERE layer_hash=$1`,
		layerHash).Scan(&storedHash)
	if !storedHash.Valid || storedHash.String == "" {
		return false // no stored hash yet, will populate on next build
	}
	return storedHash.String != currentHash
}

// EnqueueChainBuild enqueues builds for a layer chain starting from startDepth.
// configID is stored on each build row so processQueuedBuilds can resolve
// tier and credentials with a simple JOIN instead of a recursive CTE.
// Returns the number of builds enqueued.
func (s *LayerBuildScheduler) EnqueueChainBuild(ctx context.Context, layers []snapshot.LayerMaterialized, startDepth int, buildType string, configID string, force ...bool) (int, error) {
	isForce := len(force) > 0 && force[0]
	isClean := len(force) > 1 && force[1]
	enqueued := 0

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

		// For init builds (non-force, non-clean), skip layers that already have a current_version.
		// Force and refresh builds always rebuild.
		if buildType == "init" && !isForce && !isClean {
			var currentVersion sql.NullString
			s.db.QueryRowContext(ctx,
				`SELECT current_version FROM snapshot_layers WHERE layer_hash=$1`,
				layer.LayerHash).Scan(&currentVersion)
			if currentVersion.Valid && currentVersion.String != "" {
				// For platform layers (depth 0 with base image), check if the
				// capsule-thaw-agent binary has changed since the last build. If so,
				// invalidate the cached version so it gets rebuilt with the
				// new binary.
				if layer.Depth == 0 && layer.BaseImage != "" && s.isPlatformLayerStale(ctx, layer.LayerHash) {
					s.logger.WithField("layer_hash", layer.LayerHash[:16]).
						Info("Thaw-agent binary changed, invalidating platform layer")
					s.db.ExecContext(ctx,
						`UPDATE snapshot_layers SET current_version=NULL, build_artifact_hash='' WHERE layer_hash=$1`,
						layer.LayerHash)
					// Also invalidate all descendants so they won't be skipped [H3]
					s.db.ExecContext(ctx, `
						WITH RECURSIVE descendants AS (
							SELECT layer_hash FROM snapshot_layers WHERE parent_layer_hash = $1
							UNION ALL
							SELECT sl.layer_hash FROM snapshot_layers sl
							JOIN descendants d ON sl.parent_layer_hash = d.layer_hash
						)
						UPDATE snapshot_layers SET current_version = NULL
						WHERE layer_hash IN (SELECT layer_hash FROM descendants)
					`, layer.LayerHash)
				} else if s.isAllChainDrivesStale(ctx, layer.LayerHash, configID) {
					s.logger.WithFields(logrus.Fields{
						"layer_hash": layer.LayerHash[:16],
						"config_id":  configID,
					}).Info("all_chain_drives changed, invalidating layer and descendants")
					s.db.ExecContext(ctx,
						`UPDATE snapshot_layers SET current_version=NULL, all_chain_drives_hash='' WHERE layer_hash=$1`,
						layer.LayerHash)
					s.db.ExecContext(ctx, `
						WITH RECURSIVE descendants AS (
							SELECT layer_hash FROM snapshot_layers WHERE parent_layer_hash = $1
							UNION ALL
							SELECT sl.layer_hash FROM snapshot_layers sl
							JOIN descendants d ON sl.parent_layer_hash = d.layer_hash
						)
						UPDATE snapshot_layers SET current_version = NULL
						WHERE layer_hash IN (SELECT layer_hash FROM descendants)
					`, layer.LayerHash)
				} else {
					s.logger.WithFields(logrus.Fields{
						"layer_hash":      layer.LayerHash[:16],
						"current_version": currentVersion.String,
						"depth":           layer.Depth,
					}).Info("Layer already has active version, skipping")
					continue
				}
			}
		}

		// Read per-config refresh_commands and all_chain_drives from config_layer_settings
		var clsRefreshCmdsJSON string
		var clsAllChainDrivesJSON string
		s.db.QueryRowContext(ctx,
			`SELECT COALESCE(refresh_commands::text, '[]'), COALESCE(all_chain_drives::text, '[]')
			 FROM config_layer_settings WHERE config_id=$1 AND layer_hash=$2`,
			configID, layer.LayerHash).Scan(&clsRefreshCmdsJSON, &clsAllChainDrivesJSON)
		if clsRefreshCmdsJSON == "" {
			clsRefreshCmdsJSON = "[]"
		}
		if clsAllChainDrivesJSON == "" {
			clsAllChainDrivesJSON = "[]"
		}
		hasRefreshCmds := clsRefreshCmdsJSON != "[]" && clsRefreshCmdsJSON != "null"

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
		if i == startDepth && buildType == "refresh" {
			// Refresh at the target layer: restore from its own current version
			// so extension drives (workspace, caches) are preserved.
			var selfVersion sql.NullString
			s.db.QueryRowContext(ctx,
				`SELECT current_version FROM snapshot_layers WHERE layer_hash=$1`,
				layer.LayerHash).Scan(&selfVersion)
			if selfVersion.Valid && selfVersion.String != "" {
				oldLayerHash = layer.LayerHash
				oldLayerVersion = selfVersion.String
			} else {
				// No current version — fall back to init
				effectiveBuildType = "init"
			}
		} else if i > startDepth {
			if isClean {
				// Clean build: always use init commands, no old drives.
				effectiveBuildType = "init"
			} else if isForce {
				// Force rebuild: always try to preserve extension drives from own
				// current version (regardless of whether refresh_commands exist) [M1].
				var selfVersion sql.NullString
				s.db.QueryRowContext(ctx,
					`SELECT current_version FROM snapshot_layers WHERE layer_hash=$1`,
					layer.LayerHash).Scan(&selfVersion)
				if selfVersion.Valid && selfVersion.String != "" {
					oldLayerHash = layer.LayerHash
					oldLayerVersion = selfVersion.String
				}
				if selfVersion.Valid && selfVersion.String != "" && hasRefreshCmds {
					effectiveBuildType = "refresh"
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

				if oldHash.Valid && hasRefreshCmds {
					effectiveBuildType = "reattach"
					oldLayerHash = oldHash.String
					oldLayerVersion = oldVer.String
				} else {
					effectiveBuildType = "init"
				}
			}
		}

		// Resolve base_image and runner_user for self-contained build row
		baseImage := layer.BaseImage
		runnerUser := ""
		if baseImage == "" {
			// For child layers, look up from the config's layer chain
			for _, l := range layers {
				if l.BaseImage != "" {
					baseImage = l.BaseImage
					break
				}
			}
		}
		// Search all layers for platform-user (set on the platform layer),
		// so child layers inherit runner_user for rootfs hash consistency.
		for _, l := range layers {
			for _, cmd := range l.InitCommands {
				if cmd.Type == "platform-user" && len(cmd.Args) > 0 {
					runnerUser = cmd.Args[0]
				}
			}
		}

		// Populate self-contained columns on the build row
		initCmdsJSON, _ := json.Marshal(layer.InitCommands)
		drivesJSON, _ := json.Marshal(layer.Drives)

		_, err = s.db.ExecContext(ctx, `
			INSERT INTO snapshot_builds (layer_hash, version, status, build_type, parent_version,
				old_layer_hash, old_layer_version, config_id,
				init_commands, refresh_commands, drives, all_chain_drives, base_image, runner_user)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (layer_hash, version) DO NOTHING
		`, layer.LayerHash, version, status, effectiveBuildType, parentVersion,
			sql.NullString{String: oldLayerHash, Valid: oldLayerHash != ""},
			sql.NullString{String: oldLayerVersion, Valid: oldLayerVersion != ""},
			configID,
			string(initCmdsJSON), clsRefreshCmdsJSON, string(drivesJSON), clsAllChainDrivesJSON,
			baseImage, runnerUser)
		if err != nil {
			return enqueued, fmt.Errorf("failed to enqueue build for layer %s: %w", layer.Name, err)
		}
		enqueued++

		s.logger.WithFields(logrus.Fields{
			"layer_hash": layer.LayerHash[:16],
			"version":    version,
			"status":     status,
			"build_type": effectiveBuildType,
			"depth":      layer.Depth,
		}).Info("Enqueued layer build")
	}
	return enqueued, nil
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
	// Use sl.status != 'inactive' instead of 'active' to unblock pending-status layers [L1].
	result, err := s.db.ExecContext(ctx, `
		UPDATE snapshot_builds sb SET
			status = 'queued',
			parent_version = parent_sl.current_version
		FROM snapshot_layers sl
		JOIN snapshot_layers parent_sl ON sl.parent_layer_hash = parent_sl.layer_hash
		WHERE sb.status = 'waiting_parent'
		  AND sb.layer_hash = sl.layer_hash
		  AND sl.status != 'inactive'
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
		       sl.parent_layer_hash,
		       sb.init_commands::text,
		       sb.refresh_commands::text,
		       sb.drives::text,
		       sb.all_chain_drives::text,
		       sl.config_name,
		       parent_sl.current_version AS parent_current_version,
		       lc.tier,
		       sb.old_layer_hash, sb.old_layer_version,
		       lc.config_json,
		       sb.retry_count, sb.max_retries,
		       sb.base_image,
		       sb.runner_user
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
		oldLayerHash         string
		oldLayerVersion      string
		configJSON           sql.NullString
		retryCount           int
		maxRetries           int
		buildBaseImage       string
		buildRunnerUser      string
	}

	var builds []buildRow
	for rows.Next() {
		var b buildRow
		var oldHash, oldVer sql.NullString
		if err := rows.Scan(&b.buildID, &b.layerHash, &b.version, &b.buildType, &b.parentVersion,
			&b.parentLayerHash, &b.initCmdsJSON, &b.refreshCmdsJSON, &b.drivesJSON, &b.allChainDrivesJSON,
			&b.configName,
			&b.parentCurrentVersion, &b.tier,
			&oldHash, &oldVer, &b.configJSON, &b.retryCount, &b.maxRetries,
			&b.buildBaseImage, &b.buildRunnerUser); err != nil {
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

		baseImage := b.buildBaseImage
		runnerUser := b.buildRunnerUser

		// Extract auth config from the layered config JSON (if present)
		authConfigJSON := ""
		if b.configJSON.Valid && b.configJSON.String != "" {
			var lcfg snapshot.LayeredConfig
			if err := json.Unmarshal([]byte(b.configJSON.String), &lcfg); err == nil && lcfg.Config.Auth != nil {
				if authBytes, err := json.Marshal(lcfg.Config.Auth); err == nil {
					authConfigJSON = string(authBytes)
				}
			}
		}

		err := s.launchLayerBuildVM(ctx, instanceName, b.layerHash, commandsJSON, b.version,
			parentWorkloadKey, parentVersion, b.allChainDrivesJSON, b.buildType,
			snapshotVCPUs, snapshotMemoryMB,
			baseImage, runnerUser, b.oldLayerHash, b.oldLayerVersion, authConfigJSON)
		if err != nil {
			s.logger.WithError(err).WithField("build_id", b.buildID).Error("Failed to launch layer build VM")
			// Clean up VM if it was partially created before the error
			s.snapshotManager.cleanupBuilderVM(ctx, instanceName)
			// Use onBuildFailed for retry logic (handles rate limits, transient errors)
			s.onBuildFailed(ctx, b.buildID, b.layerHash, err.Error(), b.retryCount, b.maxRetries)
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

	// Atomically mark the build as completed AND update the layer status
	// in a single transaction. Previously these were two separate operations
	// and a context cancellation (e.g. pod restart) between them could leave
	// the build as 'completed' but the layer stuck at 'pending'.
	artifactHash := s.snapshotManager.ThawAgentHash(ctx)

	// Read all_chain_drives from the completed build row to compute its hash
	var buildAllChainDrives string
	s.db.QueryRowContext(ctx,
		`SELECT COALESCE(all_chain_drives::text, '[]') FROM snapshot_builds WHERE build_id=$1`,
		buildID).Scan(&buildAllChainDrives)
	drivesHash := computeAllChainDrivesHash(buildAllChainDrives)

	tx, txErr := s.db.BeginTx(ctx, nil)
	if txErr != nil {
		s.logger.WithError(txErr).WithField("build_id", buildID).Error("Failed to begin completion transaction")
		return
	}
	defer tx.Rollback()

	// Only complete the build if it's still in 'running' state.
	// A concurrent force-cancel may have set it to 'cancelled'; in that case
	// we must NOT update the layer's current_version (the clean path cleared it).
	result, err2 := tx.ExecContext(ctx, `UPDATE snapshot_builds SET status='completed', completed_at=NOW() WHERE build_id=$1 AND status='running'`, buildID)
	if err2 != nil {
		s.logger.WithError(err2).WithField("build_id", buildID).Error("Failed to mark build completed")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		s.logger.WithFields(logrus.Fields{
			"build_id":   buildID,
			"layer_hash": layerHash[:16],
		}).Warn("Build was cancelled/failed by another process, skipping completion")
		return
	}

	// Update layer status and record hashes for staleness detection.
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_layers SET current_version=$1, build_artifact_hash=$3, all_chain_drives_hash=$4, status='active', updated_at=NOW() WHERE layer_hash=$2`, version, layerHash, artifactHash, drivesHash); err != nil {
		s.logger.WithError(err).WithFields(logrus.Fields{
			"build_id":   buildID,
			"layer_hash": layerHash[:16],
		}).Error("Failed to update layer status to active")
		return
	}

	if err := tx.Commit(); err != nil {
		s.logger.WithError(err).WithField("build_id", buildID).Error("Failed to commit build completion transaction")
		return
	}

	s.logger.WithFields(logrus.Fields{
		"build_id":   buildID,
		"layer_hash": layerHash[:16],
		"version":    version,
	}).Info("Build completed and layer activated")

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
		INSERT INTO snapshots (version, status, workload_key, gcs_path, repo_commit, size_bytes, metrics)
		VALUES ($1, 'ready', $2, '', '', 0, $3)
		ON CONFLICT (version) DO NOTHING
	`, version, workloadKey, string(metricsJSON))

	if autoRollout {
		s.logger.WithField("version", version).Info("Auto-rollout: setting active snapshot")
		s.snapshotManager.SetActiveSnapshotForKey(ctx, workloadKey, version)
		if err := s.snapshotManager.AssignVersion(ctx, workloadKey, nil, version); err != nil {
			return fmt.Errorf("failed to assign fleet-wide desired version: %w", err)
		}
	}

	// Clean up draining workload_keys for configs that use this leaf
	var configID string
	err := s.db.QueryRowContext(ctx,
		`SELECT config_id FROM config_workload_keys
		 WHERE leaf_workload_key = $1 AND status = 'active'`,
		workloadKey).Scan(&configID)
	if err == nil {
		s.cleanupDrainingWorkloadKeys(ctx, configID)
	}

	return nil
}

// cleanupDrainingWorkloadKeys cleans up draining workload_keys for a config
// after a new leaf build has completed.
func (s *LayerBuildScheduler) cleanupDrainingWorkloadKeys(ctx context.Context, configID string) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT leaf_workload_key, leaf_layer_hash FROM config_workload_keys
		 WHERE config_id = $1 AND status = 'draining'`,
		configID)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var drainingWK, drainingHash string
		if err := rows.Scan(&drainingWK, &drainingHash); err != nil {
			continue
		}

		s.logger.WithFields(logrus.Fields{
			"config_id":   configID,
			"draining_wk": drainingWK,
		}).Info("Cleaning up draining workload key")

		// Check if another config still uses this workload_key actively
		var otherCount int
		s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM config_workload_keys
			 WHERE leaf_workload_key = $1 AND status = 'active'`,
			drainingWK).Scan(&otherCount)

		if otherCount == 0 {
			// No active config uses this workload_key — safe to clean up
			s.db.ExecContext(ctx,
				`DELETE FROM version_assignments WHERE workload_key = $1`, drainingWK)
			s.db.ExecContext(ctx,
				`UPDATE snapshots SET status='deprecated'
				 WHERE workload_key = $1 AND status = 'active'`, drainingWK)
		}

		// Deactivate orphaned layers from the old chain
		s.deactivateOrphanedLayers(ctx, drainingHash)

		// Remove the draining entry
		s.db.ExecContext(ctx,
			`DELETE FROM config_workload_keys
			 WHERE config_id = $1 AND leaf_workload_key = $2`,
			configID, drainingWK)
	}
}

// deactivateOrphanedLayers walks the parent chain from a leaf layer hash
// and deactivates any layers not referenced by active configs.
func (s *LayerBuildScheduler) deactivateOrphanedLayers(ctx context.Context, leafLayerHash string) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE chain AS (
			SELECT layer_hash, parent_layer_hash FROM snapshot_layers WHERE layer_hash = $1
			UNION ALL
			SELECT sl.layer_hash, sl.parent_layer_hash
			FROM snapshot_layers sl JOIN chain c ON sl.layer_hash = c.parent_layer_hash
		)
		SELECT layer_hash FROM chain
	`, leafLayerHash)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var layerHash string
		rows.Scan(&layerHash)

		// Check if still referenced by any active config
		var refCount int
		s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM config_layer_settings
			WHERE layer_hash = $1
			  AND config_id IN (SELECT config_id FROM layered_configs)
		`, layerHash).Scan(&refCount)

		if refCount == 0 {
			s.db.ExecContext(ctx,
				`UPDATE snapshot_layers SET status='inactive', current_version=NULL
				 WHERE layer_hash=$1`, layerHash)
			s.db.ExecContext(ctx,
				`UPDATE snapshot_builds SET status='cancelled'
				 WHERE layer_hash=$1 AND status IN ('queued','waiting_parent','running')`,
				layerHash)
		}
	}
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
// Iterates config_layer_settings so each config's refresh schedule is independent [H2].
func (s *LayerBuildScheduler) checkRefreshSchedules(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cls.config_id, cls.layer_hash, cls.refresh_interval, sl.current_version,
		       sl.init_commands::text, cls.refresh_commands::text,
		       sl.drives::text, cls.all_chain_drives::text,
		       (SELECT MAX(completed_at) FROM snapshot_builds
		        WHERE layer_hash = cls.layer_hash AND status = 'completed') AS last_completed,
		       EXISTS(SELECT 1 FROM snapshot_builds
		              WHERE layer_hash = cls.layer_hash
		              AND status IN ('queued','waiting_parent','running')) AS has_active_build,
		       COALESCE((SELECT base_image FROM snapshot_builds
		                 WHERE config_id = cls.config_id AND status = 'completed' AND base_image != ''
		                 ORDER BY completed_at DESC LIMIT 1), '') AS base_image,
		       COALESCE((SELECT runner_user FROM snapshot_builds
		                 WHERE config_id = cls.config_id AND status = 'completed' AND runner_user != ''
		                 ORDER BY completed_at DESC LIMIT 1), '') AS runner_user,
		       sl.parent_layer_hash,
		       EXISTS(SELECT 1 FROM snapshot_builds
		              WHERE layer_hash = sl.parent_layer_hash
		              AND status IN ('queued','waiting_parent','running')) AS parent_has_active_build
		FROM config_layer_settings cls
		JOIN snapshot_layers sl ON cls.layer_hash = sl.layer_hash
		WHERE cls.refresh_interval != '' AND cls.refresh_interval != 'on_push'
		  AND sl.status = 'active' AND sl.current_version IS NOT NULL
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	type dueLayer struct {
		configID, layerHash, refreshInterval string
		currentVersion                       sql.NullString
		initCmdsJSON, refreshCmdsJSON        string
		drivesJSON, allChainDrivesJSON       string
		baseImage, runnerUser                string
		parentLayerHash                      sql.NullString
	}

	now := time.Now()
	var dueLayers []dueLayer

	// First pass: collect all due layers
	for rows.Next() {
		var configID, layerHash, refreshInterval string
		var currentVersion sql.NullString
		var initCmdsJSON, refreshCmdsJSON, drivesJSON, allChainDrivesJSON string
		var lastCompleted sql.NullTime
		var hasActiveBuild bool
		var baseImage, runnerUser string
		var parentLayerHash sql.NullString
		var parentHasActiveBuild bool
		if err := rows.Scan(&configID, &layerHash, &refreshInterval, &currentVersion,
			&initCmdsJSON, &refreshCmdsJSON, &drivesJSON, &allChainDrivesJSON,
			&lastCompleted, &hasActiveBuild, &baseImage, &runnerUser,
			&parentLayerHash, &parentHasActiveBuild); err != nil {
			continue
		}
		if hasActiveBuild {
			continue
		}
		// Skip child layers whose parent has an active build in progress;
		// the parent's onBuildComplete cascade will handle them.
		if parentHasActiveBuild {
			continue
		}

		interval, err := time.ParseDuration(refreshInterval)
		if err != nil {
			continue
		}

		if lastCompleted.Valid && now.Sub(lastCompleted.Time) <= interval {
			continue
		}

		dueLayers = append(dueLayers, dueLayer{
			configID: configID, layerHash: layerHash, refreshInterval: refreshInterval,
			currentVersion: currentVersion,
			initCmdsJSON:   initCmdsJSON, refreshCmdsJSON: refreshCmdsJSON,
			drivesJSON: drivesJSON, allChainDrivesJSON: allChainDrivesJSON,
			baseImage: baseImage, runnerUser: runnerUser,
			parentLayerHash: parentLayerHash,
		})
	}

	// Build lookup set of due (configID, layerHash) pairs
	dueSet := make(map[string]bool, len(dueLayers))
	for _, dl := range dueLayers {
		dueSet[dl.configID+":"+dl.layerHash] = true
	}

	// Second pass: enqueue only layers whose parent is NOT also due
	// (the parent's enqueueChildRebuilds cascade will handle children)
	for _, dl := range dueLayers {
		if dl.parentLayerHash.Valid && dl.parentLayerHash.String != "" &&
			dueSet[dl.configID+":"+dl.parentLayerHash.String] {
			s.logger.WithFields(logrus.Fields{
				"layer_hash": dl.layerHash[:16],
				"config_id":  dl.configID,
				"parent":     dl.parentLayerHash.String[:16],
			}).Debug("Skipping child refresh — parent also due, will cascade")
			continue
		}

		// Enqueue a refresh build for this layer with self-contained columns
		// and old_layer_hash/old_layer_version set to preserve extension drives [M3]
		version := fmt.Sprintf("v%s-%s-%s", now.Format("20060102-150405"), dl.layerHash[:8], fmt.Sprintf("%04d", now.Nanosecond()/1e5))
		s.db.ExecContext(ctx, `
			INSERT INTO snapshot_builds (layer_hash, version, status, build_type, config_id,
				old_layer_hash, old_layer_version,
				init_commands, refresh_commands, drives, all_chain_drives,
				base_image, runner_user)
			VALUES ($1, $2, 'queued', 'refresh', $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (layer_hash, version) DO NOTHING
		`, dl.layerHash, version, dl.configID,
			sql.NullString{String: dl.layerHash, Valid: true}, dl.currentVersion,
			dl.initCmdsJSON, dl.refreshCmdsJSON, dl.drivesJSON, dl.allChainDrivesJSON,
			dl.baseImage, dl.runnerUser)

		s.logger.WithFields(logrus.Fields{
			"layer_hash": dl.layerHash[:16],
			"config_id":  dl.configID,
			"interval":   dl.refreshInterval,
		}).Info("Refresh schedule triggered")

		// Cascade: enqueue init builds for all children (scoped by config)
		s.enqueueChildRebuilds(ctx, dl.layerHash, dl.configID)
	}
}

// enqueueChildRebuilds enqueues init builds for all descendant layers.
// Scoped by config_id to only cascade within the owning config's branch [H2],
// and filters out inactive layers [M4]. Populates self-contained build columns.
func (s *LayerBuildScheduler) enqueueChildRebuilds(ctx context.Context, parentLayerHash string, configID string) {
	// Resolve base_image and runner_user from prior completed builds in this config
	var baseImage, runnerUser string
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(base_image, ''), COALESCE(runner_user, '')
		FROM snapshot_builds
		WHERE config_id = $1 AND status = 'completed' AND base_image != ''
		ORDER BY completed_at DESC LIMIT 1
	`, configID).Scan(&baseImage, &runnerUser)

	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE descendants AS (
			SELECT sl.layer_hash, sl.parent_layer_hash
			FROM snapshot_layers sl
			JOIN config_layer_settings cls ON cls.layer_hash = sl.layer_hash AND cls.config_id = $2
			WHERE sl.parent_layer_hash = $1 AND sl.status != 'inactive'
			UNION ALL
			SELECT sl.layer_hash, sl.parent_layer_hash
			FROM snapshot_layers sl
			JOIN descendants d ON sl.parent_layer_hash = d.layer_hash
			JOIN config_layer_settings cls ON cls.layer_hash = sl.layer_hash AND cls.config_id = $2
			WHERE sl.status != 'inactive'
		)
		SELECT d.layer_hash, d.parent_layer_hash,
		       sl.init_commands::text, sl.drives::text, sl.current_version,
		       cls.refresh_commands::text, cls.all_chain_drives::text
		FROM descendants d
		JOIN snapshot_layers sl ON sl.layer_hash = d.layer_hash
		JOIN config_layer_settings cls ON cls.layer_hash = d.layer_hash AND cls.config_id = $2
		WHERE NOT EXISTS (
			SELECT 1 FROM snapshot_builds sb
			WHERE sb.layer_hash = d.layer_hash
			  AND sb.status IN ('queued','waiting_parent','running')
		)
	`, parentLayerHash, configID)
	if err != nil {
		s.logger.WithError(err).Error("Failed to query descendant layers")
		return
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var childHash string
		var childParentHash sql.NullString
		var initCmdsJSON, drivesJSON string
		var currentVersion sql.NullString
		var refreshCmdsJSON, allChainDrivesJSON string
		if err := rows.Scan(&childHash, &childParentHash,
			&initCmdsJSON, &drivesJSON, &currentVersion,
			&refreshCmdsJSON, &allChainDrivesJSON); err != nil {
			continue
		}
		version := fmt.Sprintf("v%s-%s-%s", now.Format("20060102-150405"), childHash[:8], fmt.Sprintf("%04d", now.Nanosecond()/1e5))

		buildType := "init"
		var oldLayerHash, oldLayerVersion string

		hasRefresh := refreshCmdsJSON != "" && refreshCmdsJSON != "[]" && refreshCmdsJSON != "null"

		// If the child has its own current_version, set old_layer fields to
		// preserve extension drives regardless of build type.
		if currentVersion.Valid && currentVersion.String != "" {
			oldLayerHash = childHash
			oldLayerVersion = currentVersion.String
			if hasRefresh {
				buildType = "refresh"
			}
		} else {
			// No own version — try reattach from a sibling with the same commands
			var oldHash, oldVer sql.NullString
			s.db.QueryRowContext(ctx, `
				SELECT layer_hash, current_version FROM snapshot_layers
				WHERE init_commands = (SELECT init_commands FROM snapshot_layers WHERE layer_hash = $1)
				  AND drives = (SELECT drives FROM snapshot_layers WHERE layer_hash = $1)
				  AND parent_layer_hash = (SELECT parent_layer_hash FROM snapshot_layers WHERE layer_hash = $1)
				  AND layer_hash != $1 AND current_version IS NOT NULL
				ORDER BY updated_at DESC LIMIT 1
			`, childHash).Scan(&oldHash, &oldVer)

			if oldHash.Valid && hasRefresh {
				buildType = "reattach"
				oldLayerHash = oldHash.String
				oldLayerVersion = oldVer.String
			}
		}

		s.db.ExecContext(ctx, `
			INSERT INTO snapshot_builds (layer_hash, version, status, build_type,
				old_layer_hash, old_layer_version, config_id,
				init_commands, refresh_commands, drives, all_chain_drives,
				base_image, runner_user)
			VALUES ($1, $2, 'waiting_parent', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (layer_hash, version) DO NOTHING
		`, childHash, version, buildType,
			sql.NullString{String: oldLayerHash, Valid: oldLayerHash != ""},
			sql.NullString{String: oldLayerVersion, Valid: oldLayerVersion != ""},
			configID,
			initCmdsJSON, refreshCmdsJSON, drivesJSON, allChainDrivesJSON,
			baseImage, runnerUser)
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

// launchLayerBuildVM creates a GCE instance to build a layer snapshot.
// It builds its own startup script with all layer-specific flags instead of
// delegating to launchSnapshotBuilderVMForKey.
func (s *LayerBuildScheduler) launchLayerBuildVM(ctx context.Context, instanceName, layerHash, commandsJSON, version, parentWorkloadKey, parentVersion, drivesJSON, buildType string, snapshotVCPUs, snapshotMemoryMB int, baseImage, runnerUser, oldLayerHash, oldLayerVersion, authConfigJSON string) error {
	if s.snapshotManager.gcpProject == "" {
		s.logger.Warn("GCP project not configured, skipping VM launch")
		return nil
	}

	s.logger.WithFields(logrus.Fields{
		"instance":    instanceName,
		"layer_hash":  layerHash[:16],
		"version":     version,
		"build_type":  buildType,
		"runner_user": runnerUser,
		"base_image":  baseImage,
	}).Info("Launching layer build VM")

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create compute client: %w", err)
	}
	defer instancesClient.Close()

	sm := s.snapshotManager
	gcsBase := sm.gcsPath("build-artifacts")

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

	s.logger.WithFields(logrus.Fields{
		"layer_hash":  layerHash[:16],
		"layer_flags": layerFlags,
		"runner_user": runnerUser,
	}).Info("Layer build flags")

	// Auth config flag: pass via base64-encoded env var to avoid shell quoting issues
	authConfigSetup := ""
	if authConfigJSON != "" {
		authConfigB64 := base64.StdEncoding.EncodeToString([]byte(authConfigJSON))
		authConfigSetup = fmt.Sprintf(`
# Decode auth config from base64
AUTH_CONFIG=$(echo '%s' | base64 -d)
`, authConfigB64)
		layerFlags += ` --auth-config="$AUTH_CONFIG"`
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

# Setup KVM
modprobe kvm_intel || modprobe kvm_amd || true
chmod 666 /dev/kvm || true

# Load tun module; snapshot-builder sets up per-build network namespaces itself
modprobe tun || true

# Download snapshot-builder binary (always download fresh to pick up new deploys)
gcloud storage cp gs://%s/%s/snapshot-builder /usr/local/bin/snapshot-builder
chmod +x /usr/local/bin/snapshot-builder

# Download capsule-thaw-agent binary (needed for platform shim injection)
gcloud storage cp gs://%s/%s/capsule-thaw-agent /usr/local/bin/capsule-thaw-agent
chmod +x /usr/local/bin/capsule-thaw-agent

# Decode snapshot commands from base64 to avoid shell quoting issues
SNAPSHOT_COMMANDS=$(echo '%s' | base64 -d)
%s
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
    %s
echo "Layer build complete, shutting down..."
shutdown -h now
`, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, layerHash, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, sm.gcsBucket, gcsBase, base64.StdEncoding.EncodeToString([]byte(commandsJSON)), authConfigSetup, sm.gcsBucket, sm.gcsPrefix, snapshotVCPUs, snapshotMemoryMB, version, layerFlags)

	// Size the builder VM. Round up to a valid N2 vCPU count (powers of 2
	// starting at 2, except 1 is also valid but too small for builds).
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

	netIface := &computepb.NetworkInterface{
		Network: proto.String(networkURL),
	}
	if sm.builderSubnet != "" {
		region := sm.gcpZone[:len(sm.gcpZone)-2] // strip zone suffix (e.g. "us-central1-c" -> "us-central1")
		netIface.Subnetwork = proto.String(fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", sm.gcpProject, region, sm.builderSubnet))
	}

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
				netIface,
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
					Email:  proto.String(sm.builderServiceAccountEmail()),
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
