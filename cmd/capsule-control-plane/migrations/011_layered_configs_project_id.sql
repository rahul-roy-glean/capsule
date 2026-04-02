-- Add project_id to layered_configs so the scheduler can resolve
-- the access plane for a workload via project_access_planes.
ALTER TABLE layered_configs ADD COLUMN IF NOT EXISTS project_id TEXT;
CREATE INDEX IF NOT EXISTS idx_layered_configs_project ON layered_configs(project_id);
