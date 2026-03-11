ALTER TABLE session_snapshots
    ADD COLUMN IF NOT EXISTS restore_metadata JSONB;
