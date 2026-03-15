-- Migration 006: User-provided config_id + config_workload_keys lifecycle tracking
--
-- config_workload_keys tracks the workload_key lifecycle per config.
-- When layers change, the old workload_key is marked 'draining' and the new one
-- is 'active'. Draining entries are cleaned up when the new leaf build completes.

-- config_workload_keys: tracks workload_key lifecycle per config
CREATE TABLE IF NOT EXISTS config_workload_keys (
    config_id          VARCHAR(255) NOT NULL,
    leaf_workload_key  VARCHAR(16) NOT NULL,
    leaf_layer_hash    VARCHAR(64) NOT NULL,
    status             VARCHAR(16) NOT NULL DEFAULT 'active',  -- active, draining
    created_at         TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (config_id, leaf_workload_key)
);
CREATE INDEX IF NOT EXISTS idx_cwk_status ON config_workload_keys(status);

-- Ensure config_layer_settings exists (created by migration 005, but guard
-- against it not having been applied yet).
CREATE TABLE IF NOT EXISTS config_layer_settings (
    config_id        VARCHAR(255) NOT NULL,
    layer_hash       VARCHAR(64) NOT NULL REFERENCES snapshot_layers(layer_hash),
    config_name      VARCHAR(255) NOT NULL DEFAULT '',
    refresh_commands JSONB DEFAULT '[]',
    refresh_interval VARCHAR(64) DEFAULT '',
    all_chain_drives JSONB DEFAULT '[]',
    created_at       TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at       TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (config_id, layer_hash)
);
CREATE INDEX IF NOT EXISTS idx_cls_layer ON config_layer_settings(layer_hash);

-- Self-contained build inputs on snapshot_builds (also from migration 005)
ALTER TABLE snapshot_builds
    ADD COLUMN IF NOT EXISTS init_commands    JSONB DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS refresh_commands JSONB DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS drives           JSONB DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS all_chain_drives JSONB DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS base_image       VARCHAR(512) DEFAULT '',
    ADD COLUMN IF NOT EXISTS runner_user      VARCHAR(64) DEFAULT '';

-- Widen config_id from VARCHAR(64) to VARCHAR(255) for user-provided names
ALTER TABLE layered_configs ALTER COLUMN config_id TYPE VARCHAR(255);
ALTER TABLE config_layer_settings ALTER COLUMN config_id TYPE VARCHAR(255);
ALTER TABLE snapshot_builds ALTER COLUMN config_id TYPE VARCHAR(255);

-- Backfill config_workload_keys from existing layered_configs
INSERT INTO config_workload_keys (config_id, leaf_workload_key, leaf_layer_hash, status)
SELECT config_id, leaf_workload_key, leaf_layer_hash, 'active'
FROM layered_configs
WHERE leaf_workload_key IS NOT NULL AND leaf_layer_hash IS NOT NULL
ON CONFLICT DO NOTHING;
