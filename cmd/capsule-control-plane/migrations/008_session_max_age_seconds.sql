ALTER TABLE runners
    ADD COLUMN IF NOT EXISTS session_max_age_seconds INT DEFAULT 86400;

ALTER TABLE session_snapshots
    ADD COLUMN IF NOT EXISTS session_max_age_seconds INT DEFAULT 86400;
