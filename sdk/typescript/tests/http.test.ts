import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  CapsuleAuthError,
  CapsuleConnectionError,
  CapsuleHTTPError,
  CapsuleNotFound,
  CapsuleRateLimited,
  CapsuleRequestTimeoutError,
  CapsuleServiceUnavailable,
  HttpClient,
  ConnectionConfig,
} from "../src/index";

describe("HttpClient", () => {
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it("returns JSON bodies and injects request_id", async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ runners: [], count: 0 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ) as typeof fetch;

    const client = new HttpClient(
      ConnectionConfig.resolve({
        baseUrl: "http://testserver:8080",
        token: "test-token",
      }),
    );

    const result = await client.get("/api/v1/runners");
    expect(result.runners).toEqual([]);
    expect(result.count).toBe(0);
    expect(result.request_id).toBeTruthy();
  });

  it("maps 401 to auth error", async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "bad key" }), {
        status: 401,
        headers: { "Content-Type": "application/json" },
      }),
    ) as typeof fetch;

    const client = new HttpClient(
      ConnectionConfig.resolve({
        baseUrl: "http://testserver:8080",
        token: "test-token",
      }),
    );

    await expect(client.get("/api/v1/runners")).rejects.toBeInstanceOf(
      CapsuleAuthError,
    );
  });

  it("maps 404 to not found", async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "missing" }), {
        status: 404,
        headers: { "Content-Type": "application/json" },
      }),
    ) as typeof fetch;

    const client = new HttpClient(
      ConnectionConfig.resolve({
        baseUrl: "http://testserver:8080",
        token: "test-token",
      }),
    );

    await expect(client.get("/api/v1/runners/status")).rejects.toBeInstanceOf(
      CapsuleNotFound,
    );
  });

  it("retries GET 429 then raises rate limited", async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "rate limited" }), {
        status: 429,
        headers: {
          "Content-Type": "application/json",
          "Retry-After": "0",
        },
      }),
    ) as typeof fetch;

    const client = new HttpClient(
      ConnectionConfig.resolve({
        baseUrl: "http://testserver:8080",
        token: "test-token",
      }),
    );

    const promise = client.get("/api/v1/runners");
    const assertion = expect(promise).rejects.toBeInstanceOf(CapsuleRateLimited);
    await vi.runAllTimersAsync();
    await assertion;
    expect(globalThis.fetch).toHaveBeenCalledTimes(4);
  });

  it("retries GET 503 then raises service unavailable", async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "unavailable" }), {
        status: 503,
        headers: {
          "Content-Type": "application/json",
          "Retry-After": "0",
        },
      }),
    ) as typeof fetch;

    const client = new HttpClient(
      ConnectionConfig.resolve({
        baseUrl: "http://testserver:8080",
        token: "test-token",
      }),
    );

    const promise = client.get("/api/v1/runners/status");
    const assertion = expect(promise).rejects.toBeInstanceOf(CapsuleServiceUnavailable);
    await vi.runAllTimersAsync();
    await assertion;
    expect(globalThis.fetch).toHaveBeenCalledTimes(4);
  });

  it("does not retry POST by default", async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "internal" }), {
        status: 500,
        headers: { "Content-Type": "application/json" },
      }),
    ) as typeof fetch;

    const client = new HttpClient(
      ConnectionConfig.resolve({
        baseUrl: "http://testserver:8080",
        token: "test-token",
      }),
    );

    await expect(
      client.post("/api/v1/runners/release", { jsonBody: { runner_id: "r-1" } }),
    ).rejects.toBeInstanceOf(CapsuleHTTPError);
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
  });

  it("retries connection errors for GET", async () => {
    globalThis.fetch = vi
      .fn()
      .mockRejectedValue(new TypeError("failed to fetch")) as typeof fetch;

    const client = new HttpClient(
      ConnectionConfig.resolve({
        baseUrl: "http://testserver:8080",
        token: "test-token",
      }),
    );

    const promise = client.get("/api/v1/runners");
    const assertion = expect(promise).rejects.toBeInstanceOf(CapsuleConnectionError);
    await vi.runAllTimersAsync();
    await assertion;
    expect(globalThis.fetch).toHaveBeenCalledTimes(4);
  });

  it("maps abort-like failures to request timeout", async () => {
    globalThis.fetch = vi.fn().mockRejectedValue(
      new DOMException("Timed out", "AbortError"),
    ) as typeof fetch;

    const client = new HttpClient(
      ConnectionConfig.resolve({
        baseUrl: "http://testserver:8080",
        token: "test-token",
        requestTimeout: 1,
      }),
    );

    await expect(
      client.post("/api/v1/runners/release", { jsonBody: { runner_id: "r-1" } }),
    ).rejects.toBeInstanceOf(CapsuleRequestTimeoutError);
  });
});
