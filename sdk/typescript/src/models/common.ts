export interface Resources {
  cpu?: number;
  memory_mb?: number;
  disk_gb?: number;
}

export function ensureArray<T>(value: T[] | undefined | null): T[] {
  return Array.isArray(value) ? value : [];
}
