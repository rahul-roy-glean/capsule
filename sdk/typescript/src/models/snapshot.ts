export interface SnapshotMetrics {
  avg_analysis_time_ms?: number;
  cache_hit_ratio?: number;
  sample_count?: number;
}

export interface Snapshot {
  version?: string;
  status?: string;
  gcs_path?: string;
  repo_commit?: string;
  size_bytes?: number;
  created_at?: string;
  metrics?: SnapshotMetrics;
}

export interface SnapshotListResponse {
  snapshots: Snapshot[];
  count?: number;
  current_version?: string;
}

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

function asNumber(value: unknown): number | undefined {
  return typeof value === "number" ? value : undefined;
}

function asString(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

export function parseSnapshot(value: unknown): Snapshot {
  const raw = asRecord(value);
  const metricsRaw = asRecord(raw.metrics ?? raw.Metrics);

  return {
    version: asString(raw.version ?? raw.Version),
    status: asString(raw.status ?? raw.Status),
    gcs_path: asString(raw.gcs_path ?? raw.GCSPath),
    repo_commit: asString(raw.repo_commit ?? raw.RepoCommit),
    size_bytes: asNumber(raw.size_bytes ?? raw.SizeBytes),
    created_at: asString(raw.created_at ?? raw.CreatedAt),
    metrics:
      Object.keys(metricsRaw).length > 0
        ? {
            avg_analysis_time_ms: asNumber(metricsRaw.avg_analysis_time_ms),
            cache_hit_ratio: asNumber(metricsRaw.cache_hit_ratio),
            sample_count: asNumber(metricsRaw.sample_count),
          }
        : undefined,
  };
}

export function parseSnapshotListResponse(value: unknown): SnapshotListResponse {
  const raw = asRecord(value);
  const snapshots = Array.isArray(raw.snapshots) ? raw.snapshots.map((item) => parseSnapshot(item)) : [];
  return {
    snapshots,
    count: asNumber(raw.count),
    current_version: asString(raw.current_version),
  };
}
