import { afterEach, describe, expect, it } from "vitest";

import { CapsuleAuthError, CapsuleClient, CapsuleHTTPError, CapsuleNotFound } from "../src/index";

const baseUrl = (process.env.CAPSULE_BASE_URL ?? "http://localhost:8080").replace(/\/+$/u, "");
const token = process.env.CAPSULE_TOKEN ?? "test-token";

function layeredConfigBody(name: string): Record<string, unknown> {
  return {
    display_name: name,
    base_image: "ubuntu:22.04",
    layers: [
      {
        name: "workspace",
        init_commands: [
          {
            type: "shell",
            args: ["bash", "-lc", `echo ${name} > /workspace/${name}.txt`],
          },
        ],
      },
    ],
    config: {
      tier: "m",
      auto_rollout: false,
    },
  };
}

describe("TypeScript SDK contract", () => {
  afterEach(async () => {
    // no-op placeholder to keep test structure parallel with python contract style
  });

  it("requires auth", async () => {
    const client = new CapsuleClient({ baseUrl, token: "" });
    await expect(client.runners.list()).rejects.toBeInstanceOf(CapsuleAuthError);
  });

  it("accepts auth and lists runners", async () => {
    const client = new CapsuleClient({ baseUrl, token });
    const runners = await client.runners.list();
    expect(Array.isArray(runners)).toBe(true);
  });

  it("supports layered config CRUD through workloads", async () => {
    const client = new CapsuleClient({ baseUrl, token });
    const name = `ts-contract-${Math.random().toString(16).slice(2, 10)}`;
    const created = await client.workloads.onboard(layeredConfigBody(name), { build: false });

    expect(created.config_id).toBeTruthy();
    expect(created.workload_key).toBeTruthy();

    try {
      const listedIds = new Set(
        (await client.workloads.list())
          .map((cfg) => cfg.config_id)
          .filter((value): value is string => typeof value === "string" && value.length > 0),
      );
      expect(listedIds.has(created.config_id!)).toBe(true);

      const detail = await client.workloads.get(name);
      expect(detail.config_id).toBe(created.config_id);
      expect(detail.display_name).toBe(name);
    } finally {
      await client.workloads.delete(name).catch(() => undefined);
    }

    await expect(client.workloads.get(name)).rejects.toBeInstanceOf(CapsuleNotFound);
  });

  it("returns HTTP 400 for invalid layered configs", async () => {
    const client = new CapsuleClient({ baseUrl, token });

    await expect(
      client.workloads.onboard(
        {
          display_name: "invalid",
          layers: [{ name: "", init_commands: [] }],
        },
        { build: false },
      ),
    ).rejects.toSatisfy((error: unknown) => {
      return error instanceof CapsuleHTTPError && error.statusCode === 400;
    });
  });
});
