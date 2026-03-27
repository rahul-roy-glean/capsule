import { describe, expect, test } from "vitest";

import { CapsuleClient } from "../src/client";
import { Workloads } from "../src/resources/workloads";

describe("CapsuleClient surface", () => {
  test("workloads is the primary high level surface", () => {
    const client = new CapsuleClient({
      baseUrl: "http://testserver:8080",
      token: "test-token",
    });

    try {
      expect(client.workloads).toBeInstanceOf(Workloads);
    } finally {
      client.close();
    }
  });

  test("layeredConfigs is not exposed as a public documented surface", () => {
    const client = new CapsuleClient({
      baseUrl: "http://testserver:8080",
      token: "test-token",
    });

    try {
      expect("layeredConfigs" in client).toBe(true);
      expect("layered_configs" in client).toBe(false);
    } finally {
      client.close();
    }
  });
});
