import { normalizeSnapshotCommands } from "../snapshot-commands";

export interface DriveSpec {
  drive_id: string;
  label?: string;
  size_gb?: number;
  mount_path?: string;
}

export interface LayerDef {
  name: string;
  init_commands: Array<Record<string, unknown>>;
  refresh_commands?: Array<Record<string, unknown>>;
  drives?: Array<DriveSpec>;
  refresh_interval?: string;
}

export interface LayeredConfigConfig {
  auto_pause?: boolean;
  ttl?: number;
  tier?: string;
  auto_rollout?: boolean;
  session_max_age_seconds?: number;
  rootfs_size_gb?: number;
  runner_user?: string;
  workspace_size_gb?: number;
  network_policy_preset?: string;
  network_policy?: unknown;
  auth?: unknown;
}

export interface StoredLayeredConfig {
  config_id: string;
  display_name?: string;
  leaf_layer_hash?: string;
  leaf_workload_key?: string;
  tier?: string;
  start_command?: Record<string, unknown>;
  runner_ttl_seconds?: number;
  session_max_age_seconds?: number;
  auto_pause?: boolean;
  auto_rollout?: boolean;
  max_concurrent_runners?: number;
  build_schedule?: string;
  network_policy_preset?: string;
  network_policy?: unknown;
  created_at?: string;
  updated_at?: string;
}

export interface LayerStatus {
  name: string;
  layer_hash?: string;
  status?: string;
  current_version?: string;
  depth?: number;
  build_status?: string;
  build_version?: string;
}

export interface LayeredConfigDetail {
  config: StoredLayeredConfig;
  layers?: Array<LayerStatus>;
  definition?: unknown;
}

export interface CreateConfigResponse {
  config_id: string;
  leaf_workload_key?: string;
  layers?: Array<Record<string, unknown>>;
}

export interface BuildResponse {
  config_id: string;
  status?: string;
  force?: string;
  clean?: string;
}

export interface RefreshResponse {
  config_id: string;
  layer_name?: string;
  status?: string;
}

export interface LayeredConfigListResponse {
  configs: Array<StoredLayeredConfig>;
  count?: number;
}

export function normalizeLayerDef(layer: LayerDef): LayerDef {
  return {
    ...layer,
    init_commands: normalizeSnapshotCommands(layer.init_commands) ?? [],
    refresh_commands:
      normalizeSnapshotCommands(layer.refresh_commands as
        | Array<string | Record<string, unknown>>
        | undefined),
  };
}
