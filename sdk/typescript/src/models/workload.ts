export interface WorkloadSummary {
  display_name: string;
  config_id?: string | null;
  workload_key?: string | null;
}

export interface ResolvedWorkloadRef {
  display_name?: string | null;
  config_id?: string | null;
  workload_key?: string | null;
}

export function createResolvedWorkloadRef(
  value: ResolvedWorkloadRef = {},
): ResolvedWorkloadRef {
  return {
    display_name: value.display_name ?? undefined,
    config_id: value.config_id ?? undefined,
    workload_key: value.workload_key ?? undefined,
  };
}

export function createWorkloadSummary(
  value: WorkloadSummary,
): WorkloadSummary {
  return {
    display_name: value.display_name,
    config_id: value.config_id ?? undefined,
    workload_key: value.workload_key ?? undefined,
  };
}

