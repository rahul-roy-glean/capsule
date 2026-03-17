ALTER TABLE snapshot_layers ADD COLUMN IF NOT EXISTS all_chain_drives_hash VARCHAR(64) DEFAULT '';
