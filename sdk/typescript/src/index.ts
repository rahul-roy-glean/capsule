export { CapsuleClient } from "./client";
export { ConnectionConfig, type ConnectionOptions, type ResolvedConnectionConfig } from "./config";
export { HttpClient, type JsonObject, type RequestOptions, type RetryPolicy } from "./http";
export {
  CapsuleAllocationTimeoutError,
  CapsuleAuthError,
  CapsuleConflict,
  CapsuleConnectionError,
  CapsuleError,
  CapsuleHTTPError,
  CapsuleNotFound,
  CapsuleOperationTimeoutError,
  CapsuleRateLimited,
  CapsuleRequestTimeoutError,
  CapsuleRunnerUnavailableError,
  CapsuleServiceUnavailable,
  CapsuleTimeoutError,
} from "./errors";
export {
  RunnerConfig,
  RunnerConfigs,
} from "./runner-config";
export { RunnerSession, type ExecCallbacks } from "./runner-session";
export {
  ShellSession,
  MSG_EXIT,
  MSG_RESIZE,
  MSG_SIGNAL,
  MSG_STDIN,
  MSG_STDOUT,
} from "./shell";
export { validateConfigId } from "./validation";
export { SDK_VERSION } from "./version";

export { LayeredConfigs } from "./resources/layered-configs";
export { Runners } from "./resources/runners";
export { Snapshots } from "./resources/snapshots";
export { Workloads } from "./resources/workloads";

export type {
  BuildResponse,
  CreateConfigResponse,
  DriveSpec,
  LayerDef,
  LayeredConfigConfig,
  LayeredConfigDetail,
  LayerStatus,
  RefreshResponse,
  StoredLayeredConfig,
} from "./models/layered-config";
export type {
  FileEntry,
  FileListResult,
  FileMkdirResult,
  FileReadResult,
  FileRemoveResult,
  FileStatResult,
  FileUploadResult,
  FileWriteResult,
} from "./models/file";
export type { Snapshot, SnapshotListResponse, SnapshotMetrics } from "./models/snapshot";
export type {
  AllocateRunnerRequest,
  AllocateRunnerResponse,
  ExecEvent,
  ExecResult,
  PauseResult,
  Runner,
  RunnerListResponse,
  RunnerState,
  RunnerStatus,
} from "./models/runner";
export type { ResolvedWorkloadRef, WorkloadSummary } from "./models/workload";
