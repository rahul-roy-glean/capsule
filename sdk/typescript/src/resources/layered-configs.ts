import { CapsuleConflict, CapsuleNotFound } from "../errors";
import type { JsonObject } from "../http";
import { HttpClient } from "../http";
import type {
  BuildResponse,
  CreateConfigResponse,
  LayeredConfigDetail,
  RefreshResponse,
  StoredLayeredConfig,
} from "../models/layered-config";
import { ensureArray } from "../models/common";
import type { ResolvedWorkloadRef, WorkloadSummary } from "../models/workload";
import type { RunnerConfig } from "../runner-config";

export type LayeredConfigReference =
  | string
  | CreateConfigResponse
  | StoredLayeredConfig
  | LayeredConfigDetail
  | RunnerConfig
  | WorkloadSummary;

export class LayeredConfigs {
  public constructor(private readonly http: HttpClient) {}

  public async create(body: Record<string, unknown>): Promise<CreateConfigResponse> {
    return (await this.http.post("/api/v1/layered-configs", {
      jsonBody: body,
    })) as unknown as CreateConfigResponse;
  }

  public async list(): Promise<StoredLayeredConfig[]> {
    const payload = await this.http.get("/api/v1/layered-configs");
    const configs = payload.configs;
    return (Array.isArray(configs) ? configs : []) as StoredLayeredConfig[];
  }

  public async get(configId: string): Promise<LayeredConfigDetail> {
    return (await this.http.get(`/api/v1/layered-configs/${configId}`)) as unknown as LayeredConfigDetail;
  }

  public async delete(configId: string): Promise<void> {
    await this.http.delete(`/api/v1/layered-configs/${configId}`);
  }

  public async build(
    configId: string,
    options: { force?: boolean; clean?: boolean } = {},
  ): Promise<BuildResponse> {
    const params = new URLSearchParams();
    if (options.force) {
      params.set("force", "true");
    }
    if (options.clean) {
      params.set("clean", "true");
    }
    const suffix = params.size > 0 ? `?${params.toString()}` : "";
    return (await this.http.post(`/api/v1/layered-configs/${configId}/build${suffix}`)) as unknown as BuildResponse;
  }

  public async refreshLayer(configId: string, layerName: string): Promise<RefreshResponse> {
    return (await this.http.post(
      `/api/v1/layered-configs/${configId}/layers/${layerName}/refresh`,
    )) as unknown as RefreshResponse;
  }

  public async resolveWorkloadRef(configRef: LayeredConfigReference): Promise<ResolvedWorkloadRef> {
    const direct = extractDirectWorkloadRef(configRef);
    if (direct.workload_key) {
      return direct;
    }

    if (isLayeredConfigDetail(configRef)) {
      return this.resolveFromStoredConfig(configRef.config);
    }

    if (isStoredLayeredConfig(configRef)) {
      return this.resolveFromStoredConfig(configRef);
    }

    const refValue = extractReferenceValue(configRef);
    const configs = await this.list();

    const workloadMatches = configs.filter((config) => config.leaf_workload_key === refValue);
    if (workloadMatches.length > 0) {
      return this.resolveFromStoredConfig(workloadMatches[0]!);
    }

    const configIdMatches = configs.filter((config) => config.config_id === refValue);
    if (configIdMatches.length > 0) {
      return this.resolveFromStoredConfig(configIdMatches[0]!);
    }

    const displayNameMatches = configs.filter((config) => config.display_name === refValue);
    if (displayNameMatches.length > 1) {
      throw new CapsuleConflict(
        `Multiple layered configs share the display name ${JSON.stringify(refValue)}. Use a config_id or workload key to disambiguate.`,
      );
    }
    if (displayNameMatches.length === 1) {
      return this.resolveFromStoredConfig(displayNameMatches[0]!);
    }

    throw new CapsuleNotFound(
      `Could not resolve workload ${JSON.stringify(refValue)}. Pass a display name, config_id, create response, or workload key.`,
    );
  }

  public async resolveWorkloadKey(configRef: LayeredConfigReference): Promise<string> {
    const resolved = await this.resolveWorkloadRef(configRef);
    if (!resolved.workload_key) {
      throw new CapsuleNotFound("Resolved workload reference is missing a workload key.");
    }
    return resolved.workload_key;
  }

  private async resolveFromStoredConfig(config: StoredLayeredConfig): Promise<ResolvedWorkloadRef> {
    if (config.leaf_workload_key) {
      return {
        display_name: config.display_name,
        config_id: config.config_id,
        workload_key: config.leaf_workload_key,
      };
    }

    const detail = await this.get(config.config_id);
    if (detail.config.leaf_workload_key) {
      return {
        display_name: detail.config.display_name,
        config_id: detail.config.config_id,
        workload_key: detail.config.leaf_workload_key,
      };
    }

    throw new CapsuleNotFound(`Layered config ${JSON.stringify(config.config_id)} does not expose a workload key yet.`);
  }
}

function extractReferenceValue(configRef: LayeredConfigReference): string {
  if (typeof configRef === "string") {
    return configRef;
  }

  const json = configRef as unknown as JsonObject;
  const configId = json.config_id;
  if (typeof configId === "string" && configId.length > 0) {
    return configId;
  }

  const displayName = json.display_name;
  if (typeof displayName === "string" && displayName.length > 0) {
    return displayName;
  }

  throw new CapsuleNotFound("Could not determine how to resolve the requested workload reference.");
}

function extractDirectWorkloadRef(configRef: LayeredConfigReference): ResolvedWorkloadRef {
  if (typeof configRef === "string") {
    return {};
  }

  const json = configRef as unknown as JsonObject;
  const workloadKey = typeof json.workload_key === "string"
    ? json.workload_key
    : typeof json.leaf_workload_key === "string"
      ? json.leaf_workload_key
      : undefined;

  return {
    workload_key: workloadKey,
    config_id: typeof json.config_id === "string" ? json.config_id : undefined,
    display_name: typeof json.display_name === "string" ? json.display_name : undefined,
  };
}

function isStoredLayeredConfig(value: LayeredConfigReference): value is StoredLayeredConfig {
  return typeof value === "object" && value !== null && "config_id" in value;
}

function isLayeredConfigDetail(value: LayeredConfigReference): value is LayeredConfigDetail {
  return typeof value === "object" && value !== null && "config" in value;
}
