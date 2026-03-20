CREATE TABLE IF NOT EXISTS session_heads (
    session_id VARCHAR(255) PRIMARY KEY,
    workload_key VARCHAR(16) NOT NULL,
    current_host_id VARCHAR(255) NOT NULL,
    current_runner_id VARCHAR(255) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    latest_generation INT NOT NULL DEFAULT 0,
    latest_manifest_path TEXT DEFAULT '',
    runner_ttl_seconds INT DEFAULT 0,
    auto_pause BOOLEAN DEFAULT false,
    network_policy_preset VARCHAR(64),
    network_policy JSONB,
    checkpoint_interval_seconds INT DEFAULT 0,
    checkpoint_quiet_window_seconds INT DEFAULT 0,
    last_checkpointed_at TIMESTAMP WITH TIME ZONE,
    last_activity_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS session_checkpoints (
    session_id VARCHAR(255) NOT NULL,
    generation INT NOT NULL,
    manifest_path TEXT NOT NULL,
    checkpoint_kind VARCHAR(32) NOT NULL DEFAULT 'manual',
    trigger_source VARCHAR(32) NOT NULL DEFAULT 'api',
    host_id VARCHAR(255) NOT NULL,
    runner_id VARCHAR(255) NOT NULL,
    snapshot_size_bytes BIGINT DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (session_id, generation)
);

CREATE INDEX IF NOT EXISTS idx_session_heads_runner ON session_heads(current_runner_id);
CREATE INDEX IF NOT EXISTS idx_session_heads_workload ON session_heads(workload_key);
CREATE INDEX IF NOT EXISTS idx_session_heads_status ON session_heads(status);
CREATE INDEX IF NOT EXISTS idx_session_checkpoints_session ON session_checkpoints(session_id, created_at DESC);

ALTER TABLE layered_configs
    ADD COLUMN IF NOT EXISTS checkpoint_interval_seconds INT DEFAULT 0,
    ADD COLUMN IF NOT EXISTS checkpoint_quiet_window_seconds INT DEFAULT 0;
