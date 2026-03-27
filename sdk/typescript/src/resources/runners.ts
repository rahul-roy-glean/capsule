import { randomUUID } from "node:crypto";
import { URLSearchParams } from "node:url";

import {
  CapsuleAllocationTimeoutError,
  CapsuleConnectionError,
  CapsuleNotFound,
  CapsuleOperationTimeoutError,
  CapsuleRateLimited,
  CapsuleRequestTimeoutError,
  CapsuleRunnerUnavailableError,
  CapsuleServiceUnavailable,
} from "../errors";
import type { RetryPolicy } from "../http";
import { HttpClient } from "../http";
import type {
  FileListResult,
  FileMkdirResult,
  FileReadResult,
  FileRemoveResult,
  FileStatResult,
  FileUploadResult,
  FileWriteResult,
} from "../models/file";
import type { CreateConfigResponse, LayeredConfigDetail, StoredLayeredConfig } from "../models/layered-config";
import {
  parseAllocateRunnerResponse,
  parseExecEvent,
  parsePauseResult,
  parseRunner,
  parseRunnerStatus,
  type AllocateRunnerResponse,
  type ExecEvent,
  type PauseResult,
  type Runner,
  type RunnerStatus,
} from "../models/runner";
import {
  createResolvedWorkloadRef,
  type ResolvedWorkloadRef,
  type WorkloadSummary,
} from "../models/workload";
import { RunnerSession } from "../runner-session";
import type { RunnerConfig } from "../runner-config";
import { ShellSession } from "../shell";
import type { LayeredConfigs } from "./layered-configs";

const ALLOCATE_REQUEST_RETRY_POLICY: RetryPolicy = {
  maxRetries: 2,
  retryStatusCodes: new Set([502, 504]),
  retryTransportErrors: true,
  retryTimeouts: true,
};

const TERMINAL_RUNNER_STATUSES = new Set([
  "terminated",
  "unavailable",
  "quarantined",
  "suspended",
  "paused",
]);

const HOST_READ_RETRY_ERRORS = [
  CapsuleConnectionError,
  CapsuleRequestTimeoutError,
  CapsuleServiceUnavailable,
] as const;

const WAIT_RETRY_ERRORS = [
  CapsuleConnectionError,
  CapsuleNotFound,
  CapsuleRateLimited,
  CapsuleRequestTimeoutError,
  CapsuleServiceUnavailable,
] as const;

export type WorkloadInput =
  | string
  | CreateConfigResponse
  | StoredLayeredConfig
  | LayeredConfigDetail
  | RunnerConfig
  | WorkloadSummary
  | ResolvedWorkloadRef;

export interface AllocateRunnerOptions {
  requestId?: string;
  labels?: Record<string, string>;
  sessionId?: string;
  networkPolicyPreset?: string;
  networkPolicyJson?: string;
  startupTimeout?: number;
  retryPollInterval?: number;
}

export interface WaitReadyOptions {
  timeout?: number;
  pollInterval?: number;
}

export interface FromConfigOptions extends AllocateRunnerOptions {
  waitReady?: boolean;
  pollInterval?: number;
}

export interface FileUploadOptions {
  mode?: string;
  perm?: string;
}

export interface FileReadOptions {
  offset?: number;
  limit?: number;
}

export interface FileListOptions {
  recursive?: boolean;
}

export interface FileRemoveOptions {
  recursive?: boolean;
}

export interface ShellOptions {
  command?: string;
  cols?: number;
  rows?: number;
}

export interface ExecOptions {
  env?: Record<string, string>;
  workingDir?: string;
  timeoutSeconds?: number;
}

export interface QuarantineOptions {
  reason?: string;
  blockEgress?: boolean;
  pauseVm?: boolean;
}

export interface UnquarantineOptions {
  unblockEgress?: boolean;
  resumeVm?: boolean;
}

export class Runners {
  private readonly hostCache = new Map<string, string>();

  public constructor(
    private readonly http: HttpClient,
    private readonly layeredConfigs?: LayeredConfigs,
  ) {}

  public setHostCache(runnerId: string, hostAddress: string): void {
    this.hostCache.set(runnerId, hostAddress);
  }

  public getCachedHost(runnerId: string): string | undefined {
    return this.hostCache.get(runnerId);
  }

  public async allocate(
    workload: WorkloadInput,
    options: AllocateRunnerOptions = {},
  ): Promise<AllocateRunnerResponse> {
    const workloadRef = await this.resolveWorkloadRef(workload);
    const workloadKey = workloadRef.workload_key;
    if (!workloadKey) {
      throw new CapsuleNotFound("Could not resolve workload key for runner allocation.");
    }

    const stableRequestId = options.requestId ?? randomUUID();
    const body: Record<string, unknown> = {
      workload_key: workloadKey,
      request_id: stableRequestId,
    };
    if (options.labels) {
      body.labels = options.labels;
    }
    if (options.sessionId) {
      body.session_id = options.sessionId;
    }
    if (options.networkPolicyPreset) {
      body.network_policy_preset = options.networkPolicyPreset;
    }
    if (options.networkPolicyJson) {
      body.network_policy_json = options.networkPolicyJson;
    }

    const budget = this.resolveStartupTimeout(options.startupTimeout);
    const deadline = Date.now() + budget * 1000;
    const retryPollInterval = options.retryPollInterval ?? 1.0;
    let attempt = 0;
    let lastError: unknown;

    while (true) {
      try {
        const payload = await this.http.post("/api/v1/runners/allocate", {
          jsonBody: body,
          requestId: stableRequestId,
          retryPolicy: ALLOCATE_REQUEST_RETRY_POLICY,
        });
        const response = parseAllocateRunnerResponse(payload);
        if (response.hostAddress) {
          this.hostCache.set(response.runnerId, response.hostAddress);
        }
        return response;
      } catch (error) {
        if (!this.isRetryableAllocationError(error)) {
          throw error;
        }
        lastError = error;
        const remainingSeconds = Math.max(0, (deadline - Date.now()) / 1000);
        if (remainingSeconds <= 0) {
          break;
        }
        const delay = Math.min(
          this.retryDelay(error, attempt, retryPollInterval),
          remainingSeconds,
        );
        await sleep(delay * 1000);
        attempt += 1;
      }
    }

    const detail = lastError ? ` Last error: ${String(lastError)}` : "";
    throw new CapsuleAllocationTimeoutError(
      `Timed out allocating runner for workload ${JSON.stringify(workloadKey)}.${detail}`,
      {
        workloadKey,
        requestId: stableRequestId,
        timeout: budget,
      },
    );
  }

  public async status(runnerId: string): Promise<RunnerStatus> {
    const payload = await this.http.get("/api/v1/runners/status", {
      params: { runner_id: runnerId },
    });
    const result = parseRunnerStatus(payload);
    if (result.hostAddress) {
      this.hostCache.set(result.runnerId, result.hostAddress);
    }
    return result;
  }

  public async list(): Promise<Runner[]> {
    const payload = await this.http.get("/api/v1/runners");
    const runners = Array.isArray(payload.runners) ? payload.runners : [];
    return runners.map((value) => parseRunner(value));
  }

  public async release(runnerId: string): Promise<boolean> {
    const payload = await this.http.post("/api/v1/runners/release", {
      jsonBody: { runner_id: runnerId },
    });
    this.hostCache.delete(runnerId);
    return payload.success === true;
  }

  public async pause(
    runnerId: string,
    options: { syncFs?: boolean } = {},
  ): Promise<PauseResult> {
    const body: Record<string, unknown> = { runner_id: runnerId };
    if (options.syncFs) {
      body.sync_fs = true;
    }
    const payload = await this.http.post("/api/v1/runners/pause", { jsonBody: body });
    return parsePauseResult(payload);
  }

  public async quarantine(
    runnerId: string,
    options: QuarantineOptions = {},
  ): Promise<Record<string, unknown>> {
    const params = new URLSearchParams({ runner_id: runnerId });
    if (options.reason) {
      params.set("reason", options.reason);
    }
    params.set("block_egress", String(options.blockEgress ?? true).toLowerCase());
    params.set("pause_vm", String(options.pauseVm ?? true).toLowerCase());
    return this.http.post(`/api/v1/runners/quarantine?${params.toString()}`);
  }

  public async unquarantine(
    runnerId: string,
    options: UnquarantineOptions = {},
  ): Promise<Record<string, unknown>> {
    const params = new URLSearchParams({
      runner_id: runnerId,
      unblock_egress: String(options.unblockEgress ?? true).toLowerCase(),
      resume_vm: String(options.resumeVm ?? true).toLowerCase(),
    });
    return this.http.post(`/api/v1/runners/unquarantine?${params.toString()}`);
  }

  public async waitReady(
    runnerId: string,
    options: WaitReadyOptions = {},
  ): Promise<RunnerStatus> {
    const pollInterval = options.pollInterval ?? 2.0;
    const budget = this.resolveStartupTimeout(options.timeout);
    const deadline = Date.now() + budget * 1000;
    let attempt = 0;
    let lastError: unknown;

    while (Date.now() < deadline) {
      try {
        const result = await this.status(runnerId);
        if (result.status === "ready") {
          return result;
        }
        if (result.error) {
          throw new CapsuleRunnerUnavailableError(result.error, {
            runnerId,
            status: result.status,
          });
        }
        if (TERMINAL_RUNNER_STATUSES.has(result.status)) {
          throw new CapsuleRunnerUnavailableError(
            `Runner ${runnerId} entered terminal state ${JSON.stringify(result.status)}`,
            { runnerId, status: result.status },
          );
        }
        await sleep(pollInterval * 1000);
      } catch (error) {
        if (!this.isWaitRetryError(error)) {
          throw error;
        }
        lastError = error;
        const remainingSeconds = Math.max(0, (deadline - Date.now()) / 1000);
        if (remainingSeconds <= 0) {
          break;
        }
        const delay = Math.min(this.retryDelay(error, attempt, pollInterval), remainingSeconds);
        await sleep(delay * 1000);
        attempt += 1;
      }
    }

    const detail = lastError ? ` Last error: ${String(lastError)}` : "";
    throw new CapsuleOperationTimeoutError(
      `Runner ${runnerId} did not become ready within ${budget}s.${detail}`,
      {
        runnerId,
        timeout: budget,
        operation: "wait_ready",
      },
    );
  }

  public async allocateReady(
    workload: WorkloadInput,
    options: AllocateRunnerOptions & { pollInterval?: number } = {},
  ): Promise<RunnerSession> {
    const workloadRef = await this.resolveWorkloadRef(workload);
    const workloadKey = workloadRef.workload_key;
    if (!workloadKey) {
      throw new CapsuleNotFound("Could not resolve workload key for runner allocation.");
    }

    const budget = this.resolveStartupTimeout(options.startupTimeout);
    const deadline = Date.now() + budget * 1000;
    const pollInterval = options.pollInterval ?? 2.0;

    const allocation = await this.allocate(workloadKey, {
      ...options,
      startupTimeout: Math.max(0, (deadline - Date.now()) / 1000),
      retryPollInterval: Math.min(1.0, pollInterval),
    });

    const session = new RunnerSession(this, allocation.runnerId, {
      hostAddress: allocation.hostAddress,
      sessionId: allocation.sessionId,
      requestId: allocation.requestId,
    });

    const remainingSeconds = Math.max(0, (deadline - Date.now()) / 1000);
    if (remainingSeconds <= 0) {
      throw new CapsuleAllocationTimeoutError(
        `Timed out before runner ${allocation.runnerId} became ready.`,
        {
          workloadKey,
          requestId: allocation.requestId,
          timeout: budget,
        },
      );
    }

    try {
      await session.waitReady({ timeout: remainingSeconds, pollInterval });
    } catch (error) {
      if (error instanceof CapsuleOperationTimeoutError) {
        throw new CapsuleAllocationTimeoutError(
          `Timed out waiting for runner ${allocation.runnerId} to become ready.`,
          {
            workloadKey,
            requestId: allocation.requestId,
            timeout: budget,
          },
        );
      }
      throw error;
    }

    return session;
  }

  public async fromConfig(
    workload: WorkloadInput,
    options: FromConfigOptions = {},
  ): Promise<RunnerSession> {
    if (options.waitReady ?? true) {
      return this.allocateReady(workload, options);
    }

    const allocation = await this.allocate(workload, options);
    return new RunnerSession(this, allocation.runnerId, {
      hostAddress: allocation.hostAddress,
      sessionId: allocation.sessionId,
      requestId: allocation.requestId,
    });
  }

  public async fileDownload(runnerId: string, path: string): Promise<Uint8Array> {
    return this.withHostReadRetry(runnerId, (host) =>
      this.http.getBytes(`/api/v1/runners/${runnerId}/files/download`, {
        baseUrl: host,
        params: { path },
      }),
    );
  }

  public async fileUpload(
    runnerId: string,
    path: string,
    data: Uint8Array,
    options: FileUploadOptions = {},
  ): Promise<FileUploadResult> {
    const host = await this.resolveHost(runnerId);
    const params: Record<string, string> = {
      path,
      mode: options.mode ?? "overwrite",
    };
    if (options.perm) {
      params.perm = options.perm;
    }
    return (await this.http.postBytes(`/api/v1/runners/${runnerId}/files/upload`, data, {
      baseUrl: host,
      params,
    })) as FileUploadResult;
  }

  public async fileRead(
    runnerId: string,
    path: string,
    options: FileReadOptions = {},
  ): Promise<FileReadResult> {
    const body: Record<string, unknown> = { path, offset: options.offset ?? 0 };
    if (options.limit !== undefined) {
      body.limit = options.limit;
    }
    return (await this.withHostReadRetry(runnerId, (host) =>
      this.http.postToHost(`/api/v1/runners/${runnerId}/files/read`, {
        baseUrl: host,
        jsonBody: body,
      }),
    )) as FileReadResult;
  }

  public async fileWrite(
    runnerId: string,
    path: string,
    content: string,
    options: { mode?: string } = {},
  ): Promise<FileWriteResult> {
    const host = await this.resolveHost(runnerId);
    return (await this.http.postToHost(`/api/v1/runners/${runnerId}/files/write`, {
      baseUrl: host,
      jsonBody: { path, content, mode: options.mode ?? "overwrite" },
    })) as FileWriteResult;
  }

  public async fileList(
    runnerId: string,
    path: string,
    options: FileListOptions = {},
  ): Promise<FileListResult> {
    return (await this.withHostReadRetry(runnerId, (host) =>
      this.http.postToHost(`/api/v1/runners/${runnerId}/files/list`, {
        baseUrl: host,
        jsonBody: { path, recursive: options.recursive ?? false },
      }),
    )) as unknown as FileListResult;
  }

  public async fileStat(runnerId: string, path: string): Promise<FileStatResult> {
    return (await this.withHostReadRetry(runnerId, (host) =>
      this.http.postToHost(`/api/v1/runners/${runnerId}/files/stat`, {
        baseUrl: host,
        jsonBody: { path },
      }),
    )) as FileStatResult;
  }

  public async fileRemove(
    runnerId: string,
    path: string,
    options: FileRemoveOptions = {},
  ): Promise<FileRemoveResult> {
    const host = await this.resolveHost(runnerId);
    return (await this.http.postToHost(`/api/v1/runners/${runnerId}/files/remove`, {
      baseUrl: host,
      jsonBody: { path, recursive: options.recursive ?? false },
    })) as FileRemoveResult;
  }

  public async fileMkdir(runnerId: string, path: string): Promise<FileMkdirResult> {
    const host = await this.resolveHost(runnerId);
    return (await this.http.postToHost(`/api/v1/runners/${runnerId}/files/mkdir`, {
      baseUrl: host,
      jsonBody: { path },
    })) as FileMkdirResult;
  }

  public shell(runnerId: string, options: ShellOptions = {}): ShellSession {
    const query: Record<string, string | number> = {
      cols: options.cols ?? 80,
      rows: options.rows ?? 24,
    };
    if (options.command) {
      query.command = options.command;
    }

    return new ShellSession("", {
      reconnectUrlFactory: () => this.refreshShellWsUrl(runnerId, query),
      connectTimeoutMs: this.http.operationTimeoutMs,
    });
  }

  public async *exec(
    runnerId: string,
    command: string[],
    options: ExecOptions = {},
  ): AsyncIterable<ExecEvent> {
    const body: Record<string, unknown> = { command };
    if (options.env) {
      body.env = options.env;
    }
    if (options.workingDir) {
      body.working_dir = options.workingDir;
    }
    if (options.timeoutSeconds) {
      body.timeout_seconds = options.timeoutSeconds;
    }

    for await (const event of this.execWithHostRetry(runnerId, body)) {
      yield event;
    }
  }

  private resolveStartupTimeout(timeout?: number): number {
    return timeout ?? this.http.startupTimeoutMs / 1000;
  }

  private async resolveHost(runnerId: string): Promise<string> {
    const cached = this.hostCache.get(runnerId);
    if (cached) {
      return ensureScheme(cached);
    }

    const status = await this.status(runnerId);
    if (status.hostAddress) {
      this.hostCache.set(status.runnerId, status.hostAddress);
      return ensureScheme(status.hostAddress);
    }
    throw new CapsuleServiceUnavailable(`No host address available for runner ${runnerId}`);
  }

  private async *execWithHostRetry(
    runnerId: string,
    body: Record<string, unknown>,
  ): AsyncIterable<ExecEvent> {
    const url = `/api/v1/runners/${runnerId}/exec`;
    let host = await this.resolveHost(runnerId);
    let receivedAny = false;

    try {
      for await (const event of this.http.postStreamNdjson(url, {
        baseUrl: host,
        jsonBody: body,
      })) {
        receivedAny = true;
        yield parseExecEvent(event);
      }
      return;
    } catch (error) {
      if (!(error instanceof CapsuleServiceUnavailable) || receivedAny) {
        throw error;
      }
      this.hostCache.delete(runnerId);
      host = await this.resolveHost(runnerId);
      for await (const event of this.http.postStreamNdjson(url, {
        baseUrl: host,
        jsonBody: body,
      })) {
        yield parseExecEvent(event);
      }
    }
  }

  private async resolveWorkloadRef(workload: WorkloadInput): Promise<ResolvedWorkloadRef> {
    if (this.isResolvedWorkloadRef(workload)) {
      return workload;
    }

    const directWorkloadKey = getStringProperty(workload as object, "leaf_workload_key");
    if (directWorkloadKey) {
      return createResolvedWorkloadRef({
        display_name: getStringProperty(workload as object, "display_name"),
        config_id: getStringProperty(workload as object, "config_id"),
        workload_key: directWorkloadKey,
      });
    }

    if (!this.layeredConfigs) {
      if (typeof workload === "string") {
        return createResolvedWorkloadRef({
          display_name: workload,
          workload_key: workload,
        });
      }
      throw new CapsuleNotFound(
        "This runner client cannot resolve workload references without layered config support.",
      );
    }

    try {
      return await this.layeredConfigs.resolveWorkloadRef(
        workload as Exclude<WorkloadInput, ResolvedWorkloadRef>,
      );
    } catch (error) {
      if (error instanceof CapsuleNotFound && typeof workload === "string") {
        return createResolvedWorkloadRef({
          display_name: workload,
          workload_key: workload,
        });
      }
      throw error;
    }
  }

  private async withHostReadRetry<T>(
    runnerId: string,
    operation: (host: string) => Promise<T>,
  ): Promise<T> {
    const host = await this.resolveHost(runnerId);
    try {
      return await operation(host);
    } catch (error) {
      if (!this.isHostReadRetryError(error)) {
        throw error;
      }
      this.hostCache.delete(runnerId);
      return operation(await this.resolveHost(runnerId));
    }
  }

  private retryDelay(error: unknown, attempt: number, pollInterval: number): number {
    const retryAfter =
      typeof error === "object" && error !== null
        ? (error as { retryAfter?: unknown }).retryAfter
        : undefined;
    if (typeof retryAfter === "number" && retryAfter > 0) {
      return retryAfter;
    }
    return Math.max(pollInterval, Math.min(5.0, pollInterval * 2 ** attempt));
  }

  private async buildShellWsUrl(
    runnerId: string,
    query: Record<string, string | number>,
  ): Promise<string> {
    const host = await this.resolveHost(runnerId);
    const params = new URLSearchParams(
      Object.entries(query).map(([key, value]) => [key, String(value)]),
    );
    const scheme = host.startsWith("https://") ? "wss" : "ws";
    const hostAddress = host.replace(/^https?:\/\//u, "");
    return `${scheme}://${hostAddress}/api/v1/runners/${runnerId}/pty?${params.toString()}`;
  }

  private async refreshShellWsUrl(
    runnerId: string,
    query: Record<string, string | number>,
  ): Promise<string> {
    this.hostCache.delete(runnerId);
    return this.buildShellWsUrl(runnerId, query);
  }

  private isRetryableAllocationError(error: unknown): boolean {
    return (
      error instanceof CapsuleRateLimited ||
      error instanceof CapsuleServiceUnavailable ||
      error instanceof CapsuleConnectionError ||
      error instanceof CapsuleRequestTimeoutError
    );
  }

  private isWaitRetryError(error: unknown): boolean {
    return WAIT_RETRY_ERRORS.some((ctor) => error instanceof ctor);
  }

  private isHostReadRetryError(error: unknown): boolean {
    return HOST_READ_RETRY_ERRORS.some((ctor) => error instanceof ctor);
  }

  private isResolvedWorkloadRef(value: WorkloadInput): value is ResolvedWorkloadRef {
    return typeof value === "object" && value !== null && "workload_key" in value;
  }
}

function getStringProperty(value: object, key: string): string | undefined {
  const candidate = (value as Record<string, unknown>)[key];
  return typeof candidate === "string" && candidate.length > 0 ? candidate : undefined;
}

function ensureScheme(address: string): string {
  return /^https?:\/\//u.test(address) ? address : `http://${address}`;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
