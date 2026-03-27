import { beforeEach, describe, expect, it, vi } from "vitest";

import { ConnectionConfig } from "../src/config";
import {
  CapsuleAllocationTimeoutError,
  CapsuleNotFound,
  CapsuleOperationTimeoutError,
} from "../src/errors";
import { HttpClient } from "../src/http";
import { Runners } from "../src/resources/runners";
import { RunnerSession } from "../src/runner-session";

describe("Runners", () => {
  let http: HttpClient;
  let runners: Runners;

  beforeEach(() => {
    http = new HttpClient(ConnectionConfig.resolve({ baseUrl: "http://testserver:8080", token: "test-token" }));
    runners = new Runners(http);
  });

  it("allocates and caches host addresses", async () => {
    vi.spyOn(http, "post").mockResolvedValue({
      runner_id: "r-123",
      host_address: "10.0.0.1:8080",
      request_id: "req-1",
    });

    const allocation = await runners.allocate("wk-1", { requestId: "req-1" });

    expect(allocation.runnerId).toBe("r-123");
    expect(allocation.requestId).toBe("req-1");
    expect((runners as unknown as { hostCache: Map<string, string> }).hostCache.get("r-123")).toBe(
      "10.0.0.1:8080",
    );
  });

  it("preserves raw workload keys when layered lookup misses", async () => {
    const resolveWorkloadRef = vi.fn().mockRejectedValue(new CapsuleNotFound("missing"));
    const httpWithLayered = new Runners(http, { resolveWorkloadRef } as never);
    const postSpy = vi.spyOn(http, "post").mockResolvedValue({ runner_id: "r-123" });

    await httpWithLayered.allocate("wk-raw");

    expect(postSpy).toHaveBeenCalledWith(
      "/api/v1/runners/allocate",
      expect.objectContaining({
        jsonBody: expect.objectContaining({ workload_key: "wk-raw" }),
      }),
    );
  });

  it("waits until runners are ready", async () => {
    const statusSpy = vi
      .spyOn(runners, "status")
      .mockResolvedValueOnce({ runnerId: "r-1", status: "pending" })
      .mockResolvedValueOnce({ runnerId: "r-1", status: "ready" });

    const result = await runners.waitReady("r-1", { timeout: 5, pollInterval: 0 });

    expect(result.status).toBe("ready");
    expect(statusSpy).toHaveBeenCalledTimes(2);
  });

  it("converts wait timeout during allocateReady", async () => {
    vi.spyOn(runners, "allocate").mockResolvedValue({
      runnerId: "r-1",
      requestId: "req-1",
      resumed: false,
    });
    vi.spyOn(RunnerSession.prototype, "waitReady").mockRejectedValue(
      new CapsuleOperationTimeoutError("slow", { runnerId: "r-1", operation: "wait_ready" }),
    );

    await expect(runners.allocateReady("wk-1", { startupTimeout: 1 })).rejects.toBeInstanceOf(
      CapsuleAllocationTimeoutError,
    );
  });
});
