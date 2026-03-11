-- Firecracker Runner Database Schema
-- Run this against a PostgreSQL 15+ database
-- Initial baseline migration for fresh installs.

-- Enable UUID extension
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Hosts table: tracks Firecracker host VMs
CREATE TABLE IF NOT EXISTS hosts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_name VARCHAR(255) UNIQUE NOT NULL,
    zone VARCHAR(64) NOT NULL,
    status VARCHAR(32) DEFAULT 'ready' CHECK (status IN ('ready', 'draining', 'unhealthy', 'terminating')),
    idle_runners INT DEFAULT 0,
    busy_runners INT DEFAULT 0,
    snapshot_version VARCHAR(255),
    grpc_address VARCHAR(255),
    http_address VARCHAR(255),
    total_cpu_millicores INT DEFAULT 0,
    used_cpu_millicores INT DEFAULT 0,
    total_memory_mb INT DEFAULT 0,
    used_memory_mb INT DEFAULT 0,
    last_heartbeat TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Runners table: tracks individual sandbox microVMs
CREATE TABLE IF NOT EXISTS runners (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id UUID REFERENCES hosts(id) ON DELETE CASCADE,
    status VARCHAR(32) DEFAULT 'pending' CHECK (status IN ('pending', 'booting', 'initializing', 'idle', 'busy', 'draining', 'quarantined', 'retiring', 'terminated')),
    internal_ip VARCHAR(45),
    job_id VARCHAR(255),
    workload_key VARCHAR(16),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    started_at TIMESTAMP WITH TIME ZONE,
    completed_at TIMESTAMP WITH TIME ZONE
);

-- Snapshots table: tracks snapshot versions for rollout lifecycle
CREATE TABLE IF NOT EXISTS snapshots (
    version VARCHAR(255) PRIMARY KEY,
    status VARCHAR(32) DEFAULT 'building' CHECK (status IN ('building', 'ready', 'validating', 'canary', 'active', 'deprecated', 'failed', 'rolled_back')),
    gcs_path VARCHAR(512),
    workload_key VARCHAR(16),
    repo_commit VARCHAR(64),
    size_bytes BIGINT DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    metrics JSONB DEFAULT '{}'::jsonb
);

-- Version assignments: tracks which snapshot version each host should run per workload
CREATE TABLE IF NOT EXISTS version_assignments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workload_key VARCHAR(16) NOT NULL,
    host_id UUID REFERENCES hosts(id),
    version VARCHAR(255) NOT NULL,
    status VARCHAR(32) DEFAULT 'assigned',
    assigned_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    synced_at TIMESTAMP WITH TIME ZONE,
    UNIQUE(workload_key, host_id)
);

-- Jobs table: tracks CI job requests (webhook-driven allocation queue)
CREATE TABLE IF NOT EXISTS jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ci_run_id BIGINT,
    ci_job_id BIGINT,
    repo VARCHAR(512) NOT NULL,
    branch VARCHAR(255),
    commit_sha VARCHAR(64),
    status VARCHAR(32) DEFAULT 'queued' CHECK (status IN ('queued', 'assigned', 'in_progress', 'running', 'completed', 'failed', 'cancelled')),
    runner_id UUID REFERENCES runners(id) ON DELETE SET NULL,
    labels JSONB DEFAULT '[]'::jsonb,
    queued_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    started_at TIMESTAMP WITH TIME ZONE,
    completed_at TIMESTAMP WITH TIME ZONE
);

-- Session snapshots: tracks session pause/resume state
CREATE TABLE IF NOT EXISTS session_snapshots (
    session_id VARCHAR(255) PRIMARY KEY,
    workload_key VARCHAR(16) NOT NULL,
    host_id VARCHAR(255) NOT NULL,
    runner_id VARCHAR(255) NOT NULL,
    layer_count INT DEFAULT 0,
    status VARCHAR(32) DEFAULT 'active',
    restore_metadata JSONB,
    paused_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Layered snapshot pipeline tables

-- snapshot_layers: the layer DAG
CREATE TABLE IF NOT EXISTS snapshot_layers (
    layer_hash           VARCHAR(64) PRIMARY KEY,
    parent_layer_hash    VARCHAR(64) REFERENCES snapshot_layers(layer_hash),
    config_name          VARCHAR(255) NOT NULL,
    depth                INT NOT NULL DEFAULT 0,
    init_commands        JSONB NOT NULL DEFAULT '[]',
    refresh_commands     JSONB DEFAULT '[]',
    drives               JSONB DEFAULT '[]',
    all_chain_drives     JSONB DEFAULT '[]',
    refresh_interval     VARCHAR(64) DEFAULT '',
    current_version      VARCHAR(255),
    status               VARCHAR(32) DEFAULT 'pending',
    created_at           TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at           TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- snapshot_builds: the build queue
CREATE TABLE IF NOT EXISTS snapshot_builds (
    build_id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    layer_hash        VARCHAR(64) NOT NULL REFERENCES snapshot_layers(layer_hash),
    version           VARCHAR(255) NOT NULL,
    status            VARCHAR(32) DEFAULT 'queued',
    build_type        VARCHAR(16) DEFAULT 'init',
    instance_name     VARCHAR(255),
    parent_version    VARCHAR(255),
    started_at        TIMESTAMP WITH TIME ZONE,
    completed_at      TIMESTAMP WITH TIME ZONE,
    failure_reason    TEXT,
    retry_count       INT DEFAULT 0,
    max_retries       INT DEFAULT 3,
    old_layer_hash    VARCHAR(64),
    old_layer_version VARCHAR(255),
    config_id         VARCHAR(64) DEFAULT '',
    created_at        TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(layer_hash, version)
);

-- layered_configs: top-level config ownership
CREATE TABLE IF NOT EXISTS layered_configs (
    config_id              VARCHAR(64) PRIMARY KEY,
    display_name           VARCHAR(255) NOT NULL,
    config_json            TEXT NOT NULL,
    leaf_layer_hash        VARCHAR(64),
    leaf_workload_key      VARCHAR(16),
    tier                   VARCHAR(8) DEFAULT 'm',
    start_command          TEXT DEFAULT '',
    runner_ttl_seconds     INT DEFAULT 0,
    session_max_age_seconds INT DEFAULT 86400,
    auto_pause             BOOLEAN DEFAULT false,
    auto_rollout           BOOLEAN DEFAULT true,
    max_concurrent_runners INT DEFAULT 0,
    build_schedule         VARCHAR(64) DEFAULT '',
    network_policy_preset  VARCHAR(64),
    network_policy         JSONB,
    created_at             TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at             TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- repo_workload_mappings: maps CI repo names to workload keys for webhook routing
CREATE TABLE IF NOT EXISTS repo_workload_mappings (
    repo          VARCHAR(512) PRIMARY KEY,
    workload_key  VARCHAR(16) NOT NULL,
    source        VARCHAR(32) DEFAULT 'auto',
    created_at    TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- snapshot_tags: named aliases for snapshot versions
CREATE TABLE IF NOT EXISTS snapshot_tags (
    tag           VARCHAR(64) NOT NULL,
    workload_key  VARCHAR(16) NOT NULL,
    version       VARCHAR(255) NOT NULL,
    description   TEXT DEFAULT '',
    created_at    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (tag, workload_key)
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_hosts_status ON hosts(status);
CREATE INDEX IF NOT EXISTS idx_hosts_heartbeat ON hosts(last_heartbeat);
CREATE INDEX IF NOT EXISTS idx_hosts_instance_name ON hosts(instance_name);

CREATE INDEX IF NOT EXISTS idx_runners_host ON runners(host_id);
CREATE INDEX IF NOT EXISTS idx_runners_status ON runners(status);
CREATE INDEX IF NOT EXISTS idx_runners_job ON runners(job_id);

CREATE INDEX IF NOT EXISTS idx_snapshots_status ON snapshots(status);
CREATE INDEX IF NOT EXISTS idx_snapshots_created ON snapshots(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_snapshots_workload_key ON snapshots(workload_key);

CREATE INDEX IF NOT EXISTS idx_version_assignments_workload ON version_assignments(workload_key);
CREATE INDEX IF NOT EXISTS idx_version_assignments_host ON version_assignments(host_id);

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_queued ON jobs(queued_at) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS idx_jobs_runner ON jobs(runner_id);
CREATE INDEX IF NOT EXISTS idx_jobs_ci_job_id ON jobs(ci_job_id);

CREATE INDEX IF NOT EXISTS idx_layers_parent ON snapshot_layers(parent_layer_hash);
CREATE INDEX IF NOT EXISTS idx_layers_status ON snapshot_layers(status);

CREATE INDEX IF NOT EXISTS idx_builds_status ON snapshot_builds(status, created_at);
CREATE INDEX IF NOT EXISTS idx_builds_layer ON snapshot_builds(layer_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_builds_one_active_per_layer
    ON snapshot_builds (layer_hash) WHERE status IN ('queued', 'waiting_parent', 'running');

CREATE INDEX IF NOT EXISTS idx_layered_configs_leaf_wk ON layered_configs(leaf_workload_key);
CREATE INDEX IF NOT EXISTS idx_layered_configs_leaf_hash ON layered_configs(leaf_layer_hash);

CREATE INDEX IF NOT EXISTS idx_session_snapshots_workload ON session_snapshots(workload_key);

CREATE INDEX IF NOT EXISTS idx_snapshot_tags_workload ON snapshot_tags(workload_key);

-- Trigger to update updated_at on hosts
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS hosts_updated_at ON hosts;
CREATE TRIGGER hosts_updated_at
    BEFORE UPDATE ON hosts
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- Views for monitoring
CREATE OR REPLACE VIEW host_summary AS
SELECT
    COUNT(*) as total_hosts,
    COUNT(*) FILTER (WHERE status = 'ready') as ready_hosts,
    COUNT(*) FILTER (WHERE status = 'draining') as draining_hosts,
    COUNT(*) FILTER (WHERE status = 'unhealthy') as unhealthy_hosts,
    SUM(idle_runners) as idle_runners,
    SUM(busy_runners) as busy_runners
FROM hosts
WHERE last_heartbeat > NOW() - INTERVAL '2 minutes';

CREATE OR REPLACE VIEW runner_summary AS
SELECT
    status,
    COUNT(*) as count,
    AVG(EXTRACT(EPOCH FROM (NOW() - created_at))) as avg_age_seconds
FROM runners
GROUP BY status;

CREATE OR REPLACE VIEW snapshot_summary AS
SELECT
    version,
    status,
    size_bytes,
    created_at,
    metrics->>'cache_hit_ratio' as cache_hit_ratio,
    metrics->>'avg_analysis_time_ms' as avg_analysis_time_ms
FROM snapshots
ORDER BY created_at DESC
LIMIT 10;

COMMENT ON TABLE hosts IS 'GCE VMs running Firecracker and hosting microVMs';
COMMENT ON TABLE runners IS 'Individual sandbox microVMs';
COMMENT ON TABLE snapshots IS 'Firecracker snapshot versions for fast VM boot';
COMMENT ON TABLE jobs IS 'CI job queue for webhook-driven runner allocation';
COMMENT ON TABLE session_snapshots IS 'Session pause/resume state tracking';
COMMENT ON TABLE snapshot_layers IS 'Layered snapshot DAG - each layer is a set of init commands';
COMMENT ON TABLE snapshot_builds IS 'Build queue for layered snapshot pipeline';
COMMENT ON TABLE layered_configs IS 'Top-level workload configurations';
COMMENT ON TABLE repo_workload_mappings IS 'Maps CI repo names to workload keys for webhook routing';
COMMENT ON TABLE snapshot_tags IS 'Named aliases for snapshot versions (e.g., stable, canary)';
