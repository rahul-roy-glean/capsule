import { randomUUID } from "node:crypto";

import type { ResolvedConnectionConfig } from "./config";
import {
  CapsuleAuthError,
  CapsuleConflict,
  CapsuleConnectionError,
  CapsuleHTTPError,
  CapsuleNotFound,
  CapsuleRateLimited,
  CapsuleRequestTimeoutError,
  CapsuleServiceUnavailable,
} from "./errors";

const RETRYABLE_STATUS_CODES = new Set([429, 502, 503, 504]);
const MAX_RETRIES = 3;
const BASE_BACKOFF_MS = 500;

export interface RetryPolicy {
  maxRetries?: number;
  retryStatusCodes?: ReadonlySet<number>;
  retryTransportErrors?: boolean;
  retryTimeouts?: boolean;
}

const GET_RETRY_POLICY: Required<RetryPolicy> = {
  maxRetries: MAX_RETRIES,
  retryStatusCodes: RETRYABLE_STATUS_CODES,
  retryTransportErrors: true,
  retryTimeouts: true,
};

export interface RequestOptions {
  params?: Record<string, string>;
  jsonBody?: unknown;
  requestId?: string;
  timeout?: number;
  retryPolicy?: RetryPolicy;
  headers?: Record<string, string>;
}

export interface StreamOptions extends RequestOptions {
  baseUrl?: string;
}

export type JsonObject = Record<string, unknown>;

function isJsonObject(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseRetryAfter(header: string | null): number | undefined {
  if (header === null) {
    return undefined;
  }
  const parsed = Number(header);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function readJson(response: Response): Promise<JsonObject | undefined> {
  try {
    const raw = await response.json();
    return isJsonObject(raw) ? raw : undefined;
  } catch {
    return undefined;
  }
}

function backoffDelay(attempt: number, retryAfterSeconds?: number): number {
  if (retryAfterSeconds !== undefined && retryAfterSeconds > 0) {
    return retryAfterSeconds * 1000;
  }
  return BASE_BACKOFF_MS * 2 ** attempt + Math.random() * 500;
}

export class HttpClient {
  private readonly baseHeaders: Record<string, string>;

  public constructor(private readonly config: ResolvedConnectionConfig) {
    this.baseHeaders = {
      "User-Agent": config.userAgent,
      Accept: "application/json",
      ...(config.token ? { Authorization: `Bearer ${config.token}` } : {}),
    };
  }

  public get startupTimeoutMs(): number {
    return this.config.startupTimeout;
  }

  public get operationTimeoutMs(): number {
    return this.config.operationTimeout;
  }

  public async get(
    url: string,
    options: Omit<RequestOptions, "jsonBody" | "retryPolicy"> = {},
  ): Promise<JsonObject> {
    return this.request("GET", url, {
      ...options,
      retryPolicy: GET_RETRY_POLICY,
    });
  }

  public async post(url: string, options: RequestOptions = {}): Promise<JsonObject> {
    return this.request("POST", url, options);
  }

  public async delete(
    url: string,
    options: Omit<RequestOptions, "jsonBody"> = {},
  ): Promise<JsonObject> {
    return this.request("DELETE", url, options);
  }

  public async getBytes(
    url: string,
    options: Omit<StreamOptions, "jsonBody" | "retryPolicy"> = {},
  ): Promise<Uint8Array> {
    const requestId = options.requestId ?? randomUUID();
    const timeout = options.timeout ?? this.config.operationTimeout;
    const response = await this.fetchOnce(url, {
      method: "GET",
      baseUrl: options.baseUrl,
      params: options.params,
      headers: {
        ...(options.headers ?? {}),
        "X-Request-Id": requestId,
      },
      timeout,
    });

    if (!response.ok) {
      throw await this.toError(response, requestId);
    }

    return new Uint8Array(await response.arrayBuffer());
  }

  public async postBytes(
    url: string,
    data: Uint8Array,
    options: Omit<StreamOptions, "jsonBody" | "retryPolicy"> = {},
  ): Promise<JsonObject> {
    const requestId = options.requestId ?? randomUUID();
    const timeout = options.timeout ?? this.config.operationTimeout;
    const response = await this.fetchOnce(url, {
      method: "POST",
      baseUrl: options.baseUrl,
      params: options.params,
      headers: {
        ...(options.headers ?? {}),
        "X-Request-Id": requestId,
        "Content-Type": "application/octet-stream",
      },
      timeout,
      body: data,
    });

    if (!response.ok) {
      throw await this.toError(response, requestId);
    }

    return this.decodeResponseBody(response, requestId);
  }

  public async postToHost(
    url: string,
    options: Omit<StreamOptions, "retryPolicy"> = {},
  ): Promise<JsonObject> {
    const requestId = options.requestId ?? randomUUID();
    const timeout = options.timeout ?? this.config.operationTimeout;
    const response = await this.fetchOnce(url, {
      method: "POST",
      baseUrl: options.baseUrl,
      params: options.params,
      headers: {
        ...(options.headers ?? {}),
        "X-Request-Id": requestId,
      },
      timeout,
      jsonBody: options.jsonBody,
    });

    if (!response.ok) {
      throw await this.toError(response, requestId);
    }

    return this.decodeResponseBody(response, requestId);
  }

  public async *postStreamNdjson(
    url: string,
    options: Omit<StreamOptions, "retryPolicy"> = {},
  ): AsyncIterable<JsonObject> {
    const requestId = options.requestId ?? randomUUID();
    const timeout = options.timeout ?? this.config.operationTimeout;
    const response = await this.fetchOnce(url, {
      method: "POST",
      baseUrl: options.baseUrl,
      params: options.params,
      headers: {
        ...(options.headers ?? {}),
        "X-Request-Id": requestId,
        Accept: "application/x-ndjson",
      },
      timeout,
      jsonBody: options.jsonBody,
    });

    if (!response.ok) {
      throw await this.toError(response, requestId);
    }

    if (!response.body) {
      return;
    }

    const decoder = new TextDecoder();
    const reader = response.body.getReader();
    let buffered = "";

    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }

      buffered += decoder.decode(value, { stream: true });
      const lines = buffered.split("\n");
      buffered = lines.pop() ?? "";

      for (const line of lines) {
        const trimmed = line.trim();
        if (!trimmed) {
          continue;
        }
        try {
          const parsed = JSON.parse(trimmed);
          if (isJsonObject(parsed)) {
            yield parsed;
          }
        } catch {
          // Ignore malformed streaming line.
        }
      }
    }

    const trailing = buffered.trim();
    if (!trailing) {
      return;
    }
    try {
      const parsed = JSON.parse(trailing);
      if (isJsonObject(parsed)) {
        yield parsed;
      }
    } catch {
      // Ignore malformed trailing line.
    }
  }

  private async request(
    method: string,
    url: string,
    options: RequestOptions,
  ): Promise<JsonObject> {
    const requestId = options.requestId ?? randomUUID();
    const timeout = options.timeout ?? this.config.requestTimeout;
    const policy = {
      maxRetries: options.retryPolicy?.maxRetries ?? 0,
      retryStatusCodes: options.retryPolicy?.retryStatusCodes ?? new Set<number>(),
      retryTransportErrors: options.retryPolicy?.retryTransportErrors ?? false,
      retryTimeouts: options.retryPolicy?.retryTimeouts ?? false,
    };

    let lastError: unknown;
    for (let attempt = 0; attempt <= policy.maxRetries; attempt += 1) {
      try {
        const response = await this.fetchOnce(url, {
          method,
          params: options.params,
          headers: {
            ...(options.headers ?? {}),
            "X-Request-Id": requestId,
          },
          timeout,
          jsonBody: options.jsonBody,
        });

        if (response.status < 400) {
          return this.decodeResponseBody(response, requestId);
        }

        if (policy.retryStatusCodes.has(response.status) && attempt < policy.maxRetries) {
          const retryAfter = parseRetryAfter(response.headers.get("Retry-After"));
          await sleep(backoffDelay(attempt, retryAfter));
          continue;
        }

        throw await this.toError(response, requestId);
      } catch (error) {
        lastError = error;

        if (error instanceof CapsuleHTTPError) {
          throw error;
        }

        if (error instanceof Error && error.name === "AbortError") {
          if (policy.retryTimeouts && attempt < policy.maxRetries) {
            await sleep(backoffDelay(attempt));
            continue;
          }
          throw new CapsuleRequestTimeoutError(`${method} ${url} timed out`, {
            requestId,
            timeout,
            operation: "request",
          });
        }

        if (error instanceof TypeError) {
          if (policy.retryTransportErrors && attempt < policy.maxRetries) {
            await sleep(backoffDelay(attempt));
            continue;
          }
          throw new CapsuleConnectionError(error.message);
        }

        throw error;
      }
    }

    throw new CapsuleConnectionError(String(lastError));
  }

  private async fetchOnce(
    url: string,
    options: {
      method: string;
      params?: Record<string, string>;
      headers?: Record<string, string>;
      timeout: number;
      baseUrl?: string;
      jsonBody?: unknown;
      body?: Uint8Array;
    },
  ): Promise<Response> {
    const target = new URL(url, `${options.baseUrl ?? this.config.baseUrl}/`);
    for (const [key, value] of Object.entries(options.params ?? {})) {
      target.searchParams.set(key, value);
    }

    const body =
      options.body !== undefined
        ? Buffer.from(options.body)
        : options.jsonBody !== undefined
          ? JSON.stringify(options.jsonBody)
          : undefined;

    return fetch(target, {
      method: options.method,
      headers: {
        ...this.baseHeaders,
        ...(options.jsonBody !== undefined
          ? { "Content-Type": "application/json" }
          : {}),
        ...(options.headers ?? {}),
      },
      body,
      signal: AbortSignal.timeout(Math.max(1, Math.ceil(options.timeout))),
    });
  }

  private async toError(
    response: Response,
    requestId: string,
  ): Promise<CapsuleHTTPError> {
    const body = await readJson(response);
    const message =
      typeof body?.error === "string" ? body.error : await response.text();
    const details = body ? { ...body } : undefined;
    const retryAfter = parseRetryAfter(response.headers.get("Retry-After"));
    const common = { requestId, details };

    switch (response.status) {
      case 401:
        return new CapsuleAuthError(message, common);
      case 404:
        return new CapsuleNotFound(message, common);
      case 409:
        return new CapsuleConflict(message, common);
      case 429:
        return new CapsuleRateLimited(message, { ...common, retryAfter });
      case 503:
        return new CapsuleServiceUnavailable(message, {
          ...common,
          retryAfter,
        });
      default:
        return new CapsuleHTTPError(response.status, message, common);
    }
  }

  private async decodeResponseBody(
    response: Response,
    requestId: string,
  ): Promise<JsonObject> {
    const body = await readJson(response);
    if (body) {
      if (!("request_id" in body)) {
        body.request_id = requestId;
      }
      return body;
    }

    return {
      _raw: await response.text(),
      request_id: requestId,
    };
  }
}
