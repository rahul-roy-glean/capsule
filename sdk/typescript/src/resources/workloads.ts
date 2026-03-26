import fs from "node:fs";

import YAML from "js-yaml";

import { CapsuleNotFound } from "../errors";
import type {
  BuildResponse,
  CreateConfigResponse,
  LayeredConfigDetail,
  StoredLayeredConfig,
} from "../models/layered-config";
import {
  createResolvedWorkloadRef,
  createWorkloadSummary,
  type ResolvedWorkloadRef,
  type WorkloadSummary,
} from "../models/workload";
import type { RunnerConfig } from "../runner-config";
import type { RunnerSession } from "../runner-session";
import { validateConfigId } from "../validation";
import type { AllocateRunnerOptions, Runners } from "./runners";
import type { LayeredConfigs } from "./layered-configs";

export type WorkloadInput =
  | string
  | WorkloadSummary
  | CreateConfigResponse
  | StoredLayeredConfig
  | LayeredConfigDetail
  | RunnerConfig;

export interface OnboardOptions {
  name?: string;
  build?: boolean;
  force?: boolean;
  clean?: boolean;
}

export class Workloads {
  public constructor(
    private readonly layeredConfigs: LayeredConfigs,
    private readonly runners: Runners,
  ) {}

  public async onboard(
    spec: RunnerConfig | Record<string, unknown> | string,
    options: OnboardOptions = {},
  ): Promise<WorkloadSummary> {
    const body = this.normalizeSpec(spec, options.name);
    const created = await this.layeredConfigs.create(body);

    if (options.build ?? true) {
      await this.layeredConfigs.build(created.config_id, {
        force: options.force,
        clean: options.clean,
      });
    }

    return createWorkloadSummary({
      display_name: String(body.display_name),
      config_id: created.config_id,
      workload_key: created.leaf_workload_key,
    });
  }

  public async onboardYaml(yamlSpec: string, options: OnboardOptions = {}): Promise<WorkloadSummary> {
    return this.onboard(yamlSpec, options);
  }

  public async list(): Promise<WorkloadSummary[]> {
    const configs = await this.layeredConfigs.list();
    return configs.map((cfg) => this.toSummary(cfg));
  }

  public async get(workload: WorkloadInput): Promise<WorkloadSummary> {
    if (isWorkloadSummary(workload)) {
      return workload;
    }

    const resolved = await this.resolveRef(workload);
    return this.summaryFromResolved(resolved);
  }

  public async build(
    workload: WorkloadInput,
    options: { force?: boolean; clean?: boolean } = {},
  ): Promise<BuildResponse> {
    const summary = await this.get(workload);
    if (!summary.config_id) {
      throw new CapsuleNotFound(`Workload ${summary.display_name} does not have a config_id.`);
    }
    return this.layeredConfigs.build(summary.config_id, options);
  }

  public async delete(workload: WorkloadInput): Promise<void> {
    const summary = await this.get(workload);
    if (!summary.config_id) {
      throw new CapsuleNotFound(`Workload ${summary.display_name} does not have a config_id.`);
    }
    await this.layeredConfigs.delete(summary.config_id);
  }

  public async start(workload: WorkloadInput, options: AllocateRunnerOptions & { pollInterval?: number; waitReady?: boolean } = {}): Promise<RunnerSession> {
    const resolved = await this.resolveRef(workload);
    return this.runners.fromConfig(resolved, options);
  }

  public async allocate(workload: WorkloadInput, options: AllocateRunnerOptions = {}) {
    const resolved = await this.resolveRef(workload);
    return this.runners.allocate(resolved, options);
  }

  private async resolveRef(workload: WorkloadInput): Promise<ResolvedWorkloadRef> {
    if (isWorkloadSummary(workload)) {
      return createResolvedWorkloadRef(workload);
    }

    if (typeof workload === "object" && workload !== null) {
      if ("config" in workload) {
        const detail = workload as LayeredConfigDetail;
        return createResolvedWorkloadRef({
          display_name: detail.config.display_name,
          config_id: detail.config.config_id,
          workload_key: detail.config.leaf_workload_key,
        });
      }

      if ("leaf_workload_key" in workload && "config_id" in workload) {
        const config = workload as StoredLayeredConfig | CreateConfigResponse;
        return createResolvedWorkloadRef({
          display_name: "display_name" in config ? (config as StoredLayeredConfig).display_name : undefined,
          config_id: config.config_id,
          workload_key: config.leaf_workload_key,
        });
      }

      if ("workload_key" in workload) {
        return createResolvedWorkloadRef(workload as ResolvedWorkloadRef);
      }
    }

    return this.layeredConfigs.resolveWorkloadRef(workload);
  }

  private toSummary(config: StoredLayeredConfig): WorkloadSummary {
    return createWorkloadSummary({
      display_name: config.display_name ?? config.leaf_workload_key ?? config.config_id,
      config_id: config.config_id,
      workload_key: config.leaf_workload_key,
    });
  }

  private summaryFromResolved(ref: ResolvedWorkloadRef): WorkloadSummary {
    const displayName = ref.display_name ?? ref.workload_key ?? ref.config_id;
    if (!displayName) {
      throw new CapsuleNotFound("Resolved workload reference is missing a display name and identifiers.");
    }

    return createWorkloadSummary({
      display_name: displayName,
      config_id: ref.config_id,
      workload_key: ref.workload_key,
    });
  }

  private normalizeSpec(
    spec: RunnerConfig | Record<string, unknown> | string,
    providedName?: string,
  ): Record<string, unknown> {
    if (typeof spec === "object" && spec !== null && "toCreateBody" in spec && typeof spec.toCreateBody === "function") {
      return this.ensureDisplayName((spec as RunnerConfig).toCreateBody() as unknown as Record<string, unknown>, providedName);
    }

    if (typeof spec === "object" && spec !== null && !Array.isArray(spec) && !("toCreateBody" in spec)) {
      return this.normalizeMapping(spec, providedName);
    }

    const rawSpec = String(spec);
    if (rawSpec.includes("\n")) {
      return this.normalizeMapping(this.parseWorkloadYaml(rawSpec), providedName);
    }

    if (fs.existsSync(rawSpec)) {
      const text = fs.readFileSync(rawSpec, "utf8");
      return this.normalizeMapping(
        this.parseWorkloadYaml(text),
        providedName,
        rawSpec.split("/").pop()?.replace(/\.[^.]+$/u, ""),
      );
    }

    return this.normalizeMapping(this.parseWorkloadYaml(rawSpec), providedName);
  }

  private normalizeMapping(
    spec: Record<string, unknown>,
    providedName?: string,
    sourceName?: string,
  ): Record<string, unknown> {
    const workloadSpec = spec.workload;
    const body =
      workloadSpec && typeof workloadSpec === "object" && !Array.isArray(workloadSpec)
        ? { ...(workloadSpec as Record<string, unknown>) }
        : { ...spec };

    return this.ensureDisplayName(body, providedName, sourceName);
  }

  private ensureDisplayName(
    body: Record<string, unknown>,
    providedName?: string,
    sourceName?: string,
  ): Record<string, unknown> {
    const normalized = { ...body };
    const displayName = normalized.display_name ?? normalized.name ?? providedName ?? sourceName;

    if (typeof displayName !== "string" || displayName.length === 0) {
      throw new Error(
        "Workload specs must provide a config ID (slug) via `display_name`, `name`, or the `name=` argument.",
      );
    }

    validateConfigId(displayName);
    normalized.display_name = displayName;
    delete normalized.name;
    return normalized;
  }

  private parseWorkloadYaml(text: string): Record<string, unknown> {
    const loaded = YAML.load(text);
    if (typeof loaded !== "object" || loaded === null || Array.isArray(loaded)) {
      throw new Error("Workload YAML must parse to a mapping/object.");
    }
    return loaded as Record<string, unknown>;
  }
}

function isWorkloadSummary(value: WorkloadInput): value is WorkloadSummary {
  return typeof value === "object" && value !== null && "display_name" in value && !("config" in value);
}
