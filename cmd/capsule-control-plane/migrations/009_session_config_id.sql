-- Add config_id to session_snapshots for base image migration support.
-- When a config's base image changes, the workload_key changes. This column
-- lets the scheduler look up the current active workload_key for the config
-- and detect when a migration is needed on resume.
ALTER TABLE session_snapshots ADD COLUMN IF NOT EXISTS config_id VARCHAR(255);
