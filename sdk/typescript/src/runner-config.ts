import { normalizeSnapshotCommands, type SnapshotCommand } from "./snapshot-commands.js";
import { validateConfigId } from "./validation.js";
import type {
  BuildResponse,
  CreateConfigResponse,
  LayerDef,
} from "./models/layered-config.js";
import type { LayeredConfigs } from "./resources/layered-configs.js";

export type CommandLike = string | SnapshotCommand;

export interface RunnerConfigShape {
  display_name: string;
  base_image?: string;
  layers: LayerDef[];
  config?: Record<string, unknown>;
  start_command?: Record<string, unknown>;
}

export class RunnerConfig {
  readonly displayName: string;
  private readonly baseImage?: string;
  private readonly layers?: LayerDef[];
  private readonly commands: Record<string, unknown>[];
  private readonly startCommand?: Record<string, unknown>;
  private readonly autoPause?: boolean;
  private readonly ttl?: number;
  private readonly tier?: string;
  private readonly autoRollout?: boolean;
  private readonly sessionMaxAgeSeconds?: number;
  private readonly rootfsSizeGb?: number;
  private readonly runnerUser?: string;
  private readonly workspaceSizeGb?: number;
  private readonly networkPolicyPreset?: string;
  private readonly networkPolicy?: unknown;
  private readonly auth?: unknown;

  constructor(
    displayName: string,
    state?: {
      baseImage?: string;
      layers?: LayerDef[];
      commands?: Record<string, unknown>[];
      startCommand?: Record<string, unknown>;
      autoPause?: boolean;
      ttl?: number;
      tier?: string;
      autoRollout?: boolean;
      sessionMaxAgeSeconds?: number;
      rootfsSizeGb?: number;
      runnerUser?: string;
      workspaceSizeGb?: number;
      networkPolicyPreset?: string;
      networkPolicy?: unknown;
      auth?: unknown;
    },
  ) {
    this.displayName = displayName;
    this.baseImage = state?.baseImage;
    this.layers = state?.layers;
    this.commands = state?.commands ?? [];
    this.startCommand = state?.startCommand;
    this.autoPause = state?.autoPause;
    this.ttl = state?.ttl;
    this.tier = state?.tier;
    this.autoRollout = state?.autoRollout;
    this.sessionMaxAgeSeconds = state?.sessionMaxAgeSeconds;
    this.rootfsSizeGb = state?.rootfsSizeGb;
    this.runnerUser = state?.runnerUser;
    this.workspaceSizeGb = state?.workspaceSizeGb;
    this.networkPolicyPreset = state?.networkPolicyPreset;
    this.networkPolicy = state?.networkPolicy;
    this.auth = state?.auth;
  }

  withDisplayName(name: string): RunnerConfig {
    return this.clone({ displayName: name });
  }

  withBaseImage(image: string): RunnerConfig {
    return this.clone({ baseImage: image });
  }

  withLayers(layers: LayerDef[]): RunnerConfig {
    return this.clone({ layers });
  }

  withCommands(commands: CommandLike[]): RunnerConfig {
    return this.clone({ commands: normalizeSnapshotCommands(commands) ?? [] });
  }

  withStartCommand(command: Record<string, unknown>): RunnerConfig {
    return this.clone({ startCommand: command });
  }

  withAutoPause(enabled = true): RunnerConfig {
    return this.clone({ autoPause: enabled });
  }

  withTtl(seconds: number): RunnerConfig {
    return this.clone({ ttl: seconds });
  }

  withTier(tier: string): RunnerConfig {
    return this.clone({ tier });
  }

  withAutoRollout(enabled = true): RunnerConfig {
    return this.clone({ autoRollout: enabled });
  }

  withSessionMaxAge(seconds: number): RunnerConfig {
    return this.clone({ sessionMaxAgeSeconds: seconds });
  }

  withRootfsSizeGb(sizeGb: number): RunnerConfig {
    return this.clone({ rootfsSizeGb: sizeGb });
  }

  withRunnerUser(user: string): RunnerConfig {
    return this.clone({ runnerUser: user });
  }

  withWorkspaceSizeGb(sizeGb: number): RunnerConfig {
    return this.clone({ workspaceSizeGb: sizeGb });
  }

  withNetworkPolicyPreset(preset: string): RunnerConfig {
    return this.clone({ networkPolicyPreset: preset });
  }

  withNetworkPolicy(policy: unknown): RunnerConfig {
    return this.clone({ networkPolicy: policy });
  }

  withAuth(auth: unknown): RunnerConfig {
    return this.clone({ auth });
  }

  toCreateBody(): RunnerConfigShape {
    validateConfigId(this.displayName);

    const layers =
      this.layers ??
      (this.commands.length > 0
        ? [{ name: "main", init_commands: this.commands }]
        : []);

    const body: RunnerConfigShape = {
      display_name: this.displayName,
      layers,
    };

    if (this.baseImage !== undefined) {
      body.base_image = this.baseImage;
    }

    const config: Record<string, unknown> = {};
    if (this.autoPause !== undefined) config.auto_pause = this.autoPause;
    if (this.ttl !== undefined) config.ttl = this.ttl;
    if (this.tier !== undefined) config.tier = this.tier;
    if (this.autoRollout !== undefined) config.auto_rollout = this.autoRollout;
    if (this.sessionMaxAgeSeconds !== undefined) config.session_max_age_seconds = this.sessionMaxAgeSeconds;
    if (this.rootfsSizeGb !== undefined) config.rootfs_size_gb = this.rootfsSizeGb;
    if (this.runnerUser !== undefined) config.runner_user = this.runnerUser;
    if (this.workspaceSizeGb !== undefined) config.workspace_size_gb = this.workspaceSizeGb;
    if (this.networkPolicyPreset !== undefined) config.network_policy_preset = this.networkPolicyPreset;
    if (this.networkPolicy !== undefined) config.network_policy = this.networkPolicy;
    if (this.auth !== undefined) config.auth = this.auth;
    if (Object.keys(config).length > 0) {
      body.config = config;
    }

    if (this.startCommand !== undefined) {
      body.start_command = this.startCommand;
    }

    return body;
  }

  private clone(
    updates: Partial<{
      displayName: string;
      baseImage: string;
      layers: LayerDef[];
      commands: Record<string, unknown>[];
      startCommand: Record<string, unknown>;
      autoPause: boolean;
      ttl: number;
      tier: string;
      autoRollout: boolean;
      sessionMaxAgeSeconds: number;
      rootfsSizeGb: number;
      runnerUser: string;
      workspaceSizeGb: number;
      networkPolicyPreset: string;
      networkPolicy: unknown;
      auth: unknown;
    }>,
  ): RunnerConfig {
    return new RunnerConfig(updates.displayName ?? this.displayName, {
      baseImage: updates.baseImage ?? this.baseImage,
      layers: updates.layers ?? this.layers,
      commands: updates.commands ?? this.commands,
      startCommand: updates.startCommand ?? this.startCommand,
      autoPause: updates.autoPause ?? this.autoPause,
      ttl: updates.ttl ?? this.ttl,
      tier: updates.tier ?? this.tier,
      autoRollout: updates.autoRollout ?? this.autoRollout,
      sessionMaxAgeSeconds: updates.sessionMaxAgeSeconds ?? this.sessionMaxAgeSeconds,
      rootfsSizeGb: updates.rootfsSizeGb ?? this.rootfsSizeGb,
      runnerUser: updates.runnerUser ?? this.runnerUser,
      workspaceSizeGb: updates.workspaceSizeGb ?? this.workspaceSizeGb,
      networkPolicyPreset: updates.networkPolicyPreset ?? this.networkPolicyPreset,
      networkPolicy: updates.networkPolicy ?? this.networkPolicy,
      auth: updates.auth ?? this.auth,
    });
  }
}

export class RunnerConfigs {
  constructor(private readonly layeredConfigs: LayeredConfigs) {}

  apply(config: RunnerConfig): Promise<CreateConfigResponse> {
    return this.layeredConfigs.create(
      config.toCreateBody() as unknown as Record<string, unknown>,
    );
  }

  build(configId: string, options?: { force?: boolean; clean?: boolean }): Promise<BuildResponse> {
    return this.layeredConfigs.build(configId, options);
  }
}
