ALTER TABLE snapshot_layers ADD COLUMN IF NOT EXISTS build_artifact_hash VARCHAR(64) DEFAULT '';
