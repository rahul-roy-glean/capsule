ALTER TABLE runners
    ADD COLUMN IF NOT EXISTS session_id VARCHAR(255);

CREATE INDEX IF NOT EXISTS idx_runners_session ON runners(session_id);

ALTER TABLE session_snapshots
    ADD COLUMN IF NOT EXISTS parent_session_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS forked_from_runner_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS forked_at TIMESTAMP WITH TIME ZONE;

CREATE INDEX IF NOT EXISTS idx_session_snapshots_parent ON session_snapshots(parent_session_id);
