ALTER TABLE runners
    ADD COLUMN IF NOT EXISTS runner_ttl_seconds INT DEFAULT 0,
    ADD COLUMN IF NOT EXISTS auto_pause BOOLEAN DEFAULT false,
    ADD COLUMN IF NOT EXISTS network_policy_preset VARCHAR(64),
    ADD COLUMN IF NOT EXISTS network_policy JSONB;

ALTER TABLE session_snapshots
    ADD COLUMN IF NOT EXISTS runner_ttl_seconds INT DEFAULT 0,
    ADD COLUMN IF NOT EXISTS auto_pause BOOLEAN DEFAULT false,
    ADD COLUMN IF NOT EXISTS network_policy_preset VARCHAR(64),
    ADD COLUMN IF NOT EXISTS network_policy JSONB;
