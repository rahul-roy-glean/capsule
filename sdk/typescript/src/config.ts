import { SDK_VERSION } from "./version.js";

export interface ConnectionOptions {
  baseUrl?: string;
  token?: string | null;
  timeout?: number;
  requestTimeout?: number;
  startupTimeout?: number;
  operationTimeout?: number;
}

export interface ResolvedConnectionConfig {
  baseUrl: string;
  token: string | null;
  requestTimeout: number;
  startupTimeout: number;
  operationTimeout: number;
  userAgent: string;
}

export class ConnectionConfig {
  public static resolve(options: ConnectionOptions = {}): ResolvedConnectionConfig {
    const {
      baseUrl,
      token,
      timeout = 30_000,
      requestTimeout,
      startupTimeout,
      operationTimeout,
    } = options;

    return {
      baseUrl: (baseUrl ?? process.env.CAPSULE_BASE_URL ?? "http://localhost:8080").replace(/\/+$/u, ""),
      token: token ?? process.env.CAPSULE_TOKEN ?? null,
      requestTimeout: requestTimeout ?? envNumber("CAPSULE_REQUEST_TIMEOUT", timeout),
      startupTimeout: startupTimeout ?? envNumber("CAPSULE_STARTUP_TIMEOUT", 45_000),
      operationTimeout: operationTimeout ?? envNumber("CAPSULE_OPERATION_TIMEOUT", 120_000),
      userAgent: `capsule-sdk-typescript/${SDK_VERSION}`,
    };
  }
}

function envNumber(name: string, fallback: number): number {
  const raw = process.env[name];
  if (!raw) {
    return fallback;
  }

  const parsed = Number(raw);
  return Number.isFinite(parsed) ? parsed : fallback;
}
