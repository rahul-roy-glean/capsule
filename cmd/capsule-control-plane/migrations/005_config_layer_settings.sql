-- Per-config layer settings: breaks config-scoped fields out of the shared
-- snapshot_layers row so multiple configs sharing the same layer_hash do not
-- overwrite each other's refresh_commands, refresh_interval, or all_chain_drives.

CREATE TABLE IF NOT EXISTS config_layer_settings (
    config_id        VARCHAR(64) NOT NULL,
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

-- Self-contained build inputs on snapshot_builds so processQueuedBuilds does
-- not read from mutable shared snapshot_layers rows.
ALTER TABLE snapshot_builds
    ADD COLUMN IF NOT EXISTS init_commands    JSONB DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS refresh_commands JSONB DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS drives           JSONB DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS all_chain_drives JSONB DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS base_image       VARCHAR(512) DEFAULT '',
    ADD COLUMN IF NOT EXISTS runner_user      VARCHAR(64) DEFAULT '';

-- Backfill existing data into config_layer_settings from snapshot_layers
-- joined through each config's layer chain.
INSERT INTO config_layer_settings (config_id, layer_hash, config_name, refresh_commands, refresh_interval, all_chain_drives)
SELECT lc.config_id, sl.layer_hash, sl.config_name, sl.refresh_commands, sl.refresh_interval, sl.all_chain_drives
FROM layered_configs lc
CROSS JOIN LATERAL (
    WITH RECURSIVE chain AS (
        SELECT layer_hash, parent_layer_hash, config_name, refresh_commands, refresh_interval, all_chain_drives
        FROM snapshot_layers WHERE layer_hash = lc.leaf_layer_hash
        UNION ALL
        SELECT p.layer_hash, p.parent_layer_hash, p.config_name, p.refresh_commands, p.refresh_interval, p.all_chain_drives
        FROM snapshot_layers p JOIN chain c ON p.layer_hash = c.parent_layer_hash
    )
    SELECT * FROM chain
) sl
ON CONFLICT (config_id, layer_hash) DO NOTHING;
