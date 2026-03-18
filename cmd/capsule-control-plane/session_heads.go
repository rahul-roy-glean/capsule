package main

import (
	"context"
	"database/sql"
	"time"
)

type SessionHeadRecord struct {
	SessionID                    string
	WorkloadKey                  string
	CurrentHostID                string
	CurrentRunnerID              string
	Status                       string
	LatestGeneration             int
	LatestManifestPath           string
	RunnerTTLSeconds             int
	AutoPause                    bool
	NetworkPolicyPreset          string
	NetworkPolicyJSON            string
	CheckpointIntervalSeconds    int
	CheckpointQuietWindowSeconds int
	LastCheckpointedAt           time.Time
	LastActivityAt               time.Time
}

type SessionCheckpointRecord struct {
	SessionID         string
	Generation        int
	ManifestPath      string
	CheckpointKind    string
	TriggerSource     string
	HostID            string
	RunnerID          string
	SnapshotSizeBytes int64
	CreatedAt         time.Time
}

func nullableJSON(raw string) any {
	if raw == "" {
		return nil
	}
	return raw
}

func upsertSessionHead(ctx context.Context, db *sql.DB, rec SessionHeadRecord) error {
	if db == nil || rec.SessionID == "" {
		return nil
	}
	lastCheckpointedAt := any(nil)
	if !rec.LastCheckpointedAt.IsZero() {
		lastCheckpointedAt = rec.LastCheckpointedAt
	}
	lastActivityAt := any(nil)
	if !rec.LastActivityAt.IsZero() {
		lastActivityAt = rec.LastActivityAt
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO session_heads (
			session_id, workload_key, current_host_id, current_runner_id, status,
			latest_generation, latest_manifest_path, runner_ttl_seconds, auto_pause,
			network_policy_preset, network_policy, checkpoint_interval_seconds,
			checkpoint_quiet_window_seconds, last_checkpointed_at, last_activity_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (session_id) DO UPDATE SET
			workload_key = EXCLUDED.workload_key,
			current_host_id = EXCLUDED.current_host_id,
			current_runner_id = EXCLUDED.current_runner_id,
			status = EXCLUDED.status,
			latest_generation = EXCLUDED.latest_generation,
			latest_manifest_path = EXCLUDED.latest_manifest_path,
			runner_ttl_seconds = EXCLUDED.runner_ttl_seconds,
			auto_pause = EXCLUDED.auto_pause,
			network_policy_preset = COALESCE(NULLIF(EXCLUDED.network_policy_preset, ''), session_heads.network_policy_preset),
			network_policy = COALESCE(EXCLUDED.network_policy, session_heads.network_policy),
			checkpoint_interval_seconds = EXCLUDED.checkpoint_interval_seconds,
			checkpoint_quiet_window_seconds = EXCLUDED.checkpoint_quiet_window_seconds,
			last_checkpointed_at = COALESCE(EXCLUDED.last_checkpointed_at, session_heads.last_checkpointed_at),
			last_activity_at = COALESCE(EXCLUDED.last_activity_at, session_heads.last_activity_at),
			updated_at = NOW()
	`, rec.SessionID, rec.WorkloadKey, rec.CurrentHostID, rec.CurrentRunnerID, rec.Status,
		rec.LatestGeneration, rec.LatestManifestPath, rec.RunnerTTLSeconds, rec.AutoPause,
		rec.NetworkPolicyPreset, nullableJSON(rec.NetworkPolicyJSON), rec.CheckpointIntervalSeconds,
		rec.CheckpointQuietWindowSeconds, lastCheckpointedAt, lastActivityAt); err != nil {
		return err
	}

	// Maintain the legacy compatibility row until all callers migrate.
	_, err := db.ExecContext(ctx, `
		INSERT INTO session_snapshots (
			session_id, runner_id, workload_key, host_id, status, layer_count, paused_at,
			runner_ttl_seconds, auto_pause, network_policy_preset, network_policy
		)
		VALUES ($1, $2, $3, $4, $5, $6,
			CASE WHEN $5 = 'suspended' THEN COALESCE($7, NOW()) ELSE NULL END,
			$8, $9, $10, $11)
		ON CONFLICT (session_id) DO UPDATE SET
			runner_id = EXCLUDED.runner_id,
			workload_key = EXCLUDED.workload_key,
			host_id = EXCLUDED.host_id,
			status = EXCLUDED.status,
			layer_count = EXCLUDED.layer_count,
			paused_at = CASE
				WHEN EXCLUDED.status = 'suspended' THEN COALESCE(EXCLUDED.paused_at, NOW())
				ELSE session_snapshots.paused_at
			END,
			runner_ttl_seconds = EXCLUDED.runner_ttl_seconds,
			auto_pause = EXCLUDED.auto_pause,
			network_policy_preset = EXCLUDED.network_policy_preset,
			network_policy = EXCLUDED.network_policy
	`, rec.SessionID, rec.CurrentRunnerID, rec.WorkloadKey, rec.CurrentHostID, rec.Status,
		rec.LatestGeneration, lastCheckpointedAt, rec.RunnerTTLSeconds, rec.AutoPause,
		rec.NetworkPolicyPreset, nullableJSON(rec.NetworkPolicyJSON))
	return err
}

func insertSessionCheckpoint(ctx context.Context, db *sql.DB, rec SessionCheckpointRecord) error {
	if db == nil || rec.SessionID == "" || rec.Generation == 0 || rec.ManifestPath == "" {
		return nil
	}
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO session_checkpoints (
			session_id, generation, manifest_path, checkpoint_kind, trigger_source,
			host_id, runner_id, snapshot_size_bytes, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (session_id, generation) DO UPDATE SET
			manifest_path = EXCLUDED.manifest_path,
			checkpoint_kind = EXCLUDED.checkpoint_kind,
			trigger_source = EXCLUDED.trigger_source,
			host_id = EXCLUDED.host_id,
			runner_id = EXCLUDED.runner_id,
			snapshot_size_bytes = EXCLUDED.snapshot_size_bytes
	`, rec.SessionID, rec.Generation, rec.ManifestPath, rec.CheckpointKind, rec.TriggerSource,
		rec.HostID, rec.RunnerID, rec.SnapshotSizeBytes, createdAt)
	return err
}

func getSessionHeadByRunnerID(ctx context.Context, db *sql.DB, runnerID string) (*SessionHeadRecord, error) {
	if db == nil {
		return nil, sql.ErrConnDone
	}
	var rec SessionHeadRecord
	var networkPolicy sql.NullString
	var checkpointedAt sql.NullTime
	var activityAt sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT session_id, workload_key, current_host_id, current_runner_id, status,
		       latest_generation, latest_manifest_path, runner_ttl_seconds, auto_pause,
		       network_policy_preset, network_policy, checkpoint_interval_seconds,
		       checkpoint_quiet_window_seconds, last_checkpointed_at, last_activity_at
		FROM session_heads
		WHERE current_runner_id = $1
	`, runnerID).Scan(
		&rec.SessionID, &rec.WorkloadKey, &rec.CurrentHostID, &rec.CurrentRunnerID, &rec.Status,
		&rec.LatestGeneration, &rec.LatestManifestPath, &rec.RunnerTTLSeconds, &rec.AutoPause,
		&rec.NetworkPolicyPreset, &networkPolicy, &rec.CheckpointIntervalSeconds,
		&rec.CheckpointQuietWindowSeconds, &checkpointedAt, &activityAt,
	)
	if err != nil {
		return nil, err
	}
	if networkPolicy.Valid {
		rec.NetworkPolicyJSON = networkPolicy.String
	}
	if checkpointedAt.Valid {
		rec.LastCheckpointedAt = checkpointedAt.Time
	}
	if activityAt.Valid {
		rec.LastActivityAt = activityAt.Time
	}
	return &rec, nil
}

func getSessionHeadBySessionID(ctx context.Context, db *sql.DB, sessionID string) (*SessionHeadRecord, error) {
	if db == nil {
		return nil, sql.ErrConnDone
	}
	var rec SessionHeadRecord
	var networkPolicy sql.NullString
	var checkpointedAt sql.NullTime
	var activityAt sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT session_id, workload_key, current_host_id, current_runner_id, status,
		       latest_generation, latest_manifest_path, runner_ttl_seconds, auto_pause,
		       network_policy_preset, network_policy, checkpoint_interval_seconds,
		       checkpoint_quiet_window_seconds, last_checkpointed_at, last_activity_at
		FROM session_heads
		WHERE session_id = $1
	`, sessionID).Scan(
		&rec.SessionID, &rec.WorkloadKey, &rec.CurrentHostID, &rec.CurrentRunnerID, &rec.Status,
		&rec.LatestGeneration, &rec.LatestManifestPath, &rec.RunnerTTLSeconds, &rec.AutoPause,
		&rec.NetworkPolicyPreset, &networkPolicy, &rec.CheckpointIntervalSeconds,
		&rec.CheckpointQuietWindowSeconds, &checkpointedAt, &activityAt,
	)
	if err != nil {
		return nil, err
	}
	if networkPolicy.Valid {
		rec.NetworkPolicyJSON = networkPolicy.String
	}
	if checkpointedAt.Valid {
		rec.LastCheckpointedAt = checkpointedAt.Time
	}
	if activityAt.Valid {
		rec.LastActivityAt = activityAt.Time
	}
	return &rec, nil
}
