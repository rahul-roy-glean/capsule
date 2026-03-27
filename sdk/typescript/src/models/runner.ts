import type { JsonObject } from "../http";

export type RunnerState =
  | "idle"
  | "busy"
  | "booting"
  | "initializing"
  | "paused"
  | "pausing"
  | "suspended"
  | "quarantined"
  | "draining"
  | "terminated"
  | "ready"
  | "pending"
  | "unavailable";

export interface Runner {
  runnerId?: string;
  hostId?: string;
  hostAddress?: string;
  status?: string;
  internalIp?: string;
  sessionId?: string;
  resumed?: boolean;
}

export interface AllocateRunnerRequest {
  workloadKey: string;
  requestId?: string;
  labels?: Record<string, string>;
  sessionId?: string;
  networkPolicyPreset?: string;
  networkPolicyJson?: string;
}

export interface AllocateRunnerResponse {
  runnerId: string;
  hostId?: string;
  hostAddress?: string;
  internalIp?: string;
  sessionId?: string;
  resumed: boolean;
  requestId?: string;
}

export interface RunnerStatus {
  runnerId: string;
  status: string;
  hostAddress?: string;
  error?: string;
}

export interface PauseResult {
  success: boolean;
  sessionId?: string;
  snapshotSizeBytes?: number;
  layer?: number;
}

export interface RunnerListResponse {
  runners: Runner[];
  count?: number;
}

export interface ExecRequest {
  command: string[];
  env?: Record<string, string>;
  workingDir?: string;
  timeoutSeconds?: number;
}

export interface ExecEvent {
  type: string;
  data?: string;
  code?: number;
  message?: string;
  ts?: string;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  durationMs?: number;
}

function asObject(value: unknown): JsonObject {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return {};
  }
  return value as JsonObject;
}

function asString(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

function asBoolean(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

function asNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

export function parseRunner(value: unknown): Runner {
  const payload = asObject(value);
  return {
    runnerId: asString(payload.runner_id),
    hostId: asString(payload.host_id),
    hostAddress: asString(payload.host_address),
    status: asString(payload.status),
    internalIp: asString(payload.internal_ip),
    sessionId: asString(payload.session_id),
    resumed: asBoolean(payload.resumed),
  };
}

export function parseAllocateRunnerResponse(value: unknown): AllocateRunnerResponse {
  const payload = asObject(value);
  const runnerId = asString(payload.runner_id);
  if (!runnerId) {
    throw new Error("Allocate runner response is missing runner_id.");
  }
  return {
    runnerId,
    hostId: asString(payload.host_id),
    hostAddress: asString(payload.host_address),
    internalIp: asString(payload.internal_ip),
    sessionId: asString(payload.session_id),
    resumed: asBoolean(payload.resumed) ?? false,
    requestId: asString(payload.request_id),
  };
}

export function parseRunnerStatus(value: unknown): RunnerStatus {
  const payload = asObject(value);
  const runnerId = asString(payload.runner_id);
  const status = asString(payload.status);
  if (!runnerId || !status) {
    throw new Error("Runner status response is missing required fields.");
  }
  return {
    runnerId,
    status,
    hostAddress: asString(payload.host_address),
    error: asString(payload.error),
  };
}

export function parsePauseResult(value: unknown): PauseResult {
  const payload = asObject(value);
  return {
    success: asBoolean(payload.success) ?? false,
    sessionId: asString(payload.session_id),
    snapshotSizeBytes: asNumber(payload.snapshot_size_bytes),
    layer: asNumber(payload.layer),
  };
}

export function parseExecEvent(value: unknown): ExecEvent {
  const payload = asObject(value);
  const type = asString(payload.type);
  if (!type) {
    throw new Error("Exec event is missing type.");
  }
  return {
    type,
    data: asString(payload.data),
    code: asNumber(payload.code),
    message: asString(payload.message),
    ts: asString(payload.ts),
  };
}
