import { afterEach, describe, expect, it, vi } from "vitest";

import { CapsuleClient, RunnerConfig, RunnerSession } from "../src/index";

const restoreFetch = globalThis.fetch;

describe("Workloads", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    globalThis.fetch = restoreFetch;
  });

  it("onboards a runner config and triggers a build", async () => {
    globalThis.fetch = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            config_id: "my-sandbox",
            leaf_workload_key: "wk-leaf",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            config_id: "my-sandbox",
            status: "build_enqueued",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );

    const client = new CapsuleClient({
      baseUrl: "http://testserver:8080",
      token: "test-token",
    });

    const result = await client.workloads.onboard(
      new RunnerConfig("my-sandbox").withCommands(["echo hi"]),
    );

    expect(result.display_name).toBe("my-sandbox");
    expect(result.workload_key).toBe("wk-leaf");
    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
  });

  it("delegates start to runners", async () => {
    const client = new CapsuleClient({
      baseUrl: "http://testserver:8080",
      token: "test-token",
    });

    const session = new RunnerSession(client.runners, "r-1");
    const resolveRef = vi
      .spyOn(client.workloads as unknown as { resolveRef: (value: string) => Promise<unknown> }, "resolveRef")
      .mockResolvedValue({
        display_name: "My Sandbox",
        workload_key: "wk-leaf",
      });
    const fromConfig = vi.spyOn(client.runners, "fromConfig").mockResolvedValue(session);

    const result = await client.workloads.start("My Sandbox", {
      retryPollInterval: 1,
    });

    expect(result).toBe(session);
    expect(resolveRef).toHaveBeenCalledWith("My Sandbox");
    expect(fromConfig).toHaveBeenCalledWith(
      expect.objectContaining({ workload_key: "wk-leaf" }),
      { retryPollInterval: 1 },
    );
  });

  it("lists snapshots", async () => {
    globalThis.fetch = vi.fn<typeof fetch>().mockResolvedValue(
      new Response(
        JSON.stringify({
          snapshots: [
            {
              Version: "v1",
              Status: "active",
              GCSPath: "gs://bucket/v1",
              RepoCommit: "abc123",
              SizeBytes: 1024,
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    const client = new CapsuleClient({
      baseUrl: "http://testserver:8080",
      token: "test-token",
    });

    const snapshots = await client.snapshots.list();

    expect(snapshots).toHaveLength(1);
    expect(snapshots[0]?.version).toBe("v1");
    expect(snapshots[0]?.gcs_path).toBe("gs://bucket/v1");
  });

  it("allocates a runner from workloads", async () => {
    const client = new CapsuleClient({
      baseUrl: "http://testserver:8080",
      token: "test-token",
    });

    vi.spyOn(
      client.workloads as unknown as { resolveRef: (value: string) => Promise<unknown> },
      "resolveRef",
    ).mockResolvedValue({
      display_name: "My Sandbox",
      workload_key: "wk-leaf",
    });
    const allocate = vi.spyOn(client.runners, "allocate").mockResolvedValue({
      runnerId: "r-1",
      requestId: "req-1",
      resumed: false,
    });

    const result = await client.workloads.allocate("My Sandbox", {
      startupTimeout: 10,
    });

    expect(result.runnerId).toBe("r-1");
    expect(allocate).toHaveBeenCalledWith(
      expect.objectContaining({ workload_key: "wk-leaf" }),
      { startupTimeout: 10 },
    );
  });
});
