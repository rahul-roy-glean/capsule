-- Remove version_assignments table. Version resolution now uses
-- snapshot_layers.current_version (via the snapshots table's active status)
-- directly, without per-host assignment tracking.
DROP TABLE IF EXISTS version_assignments;
