import { describe, expect, it } from "vitest";

import { MSG_EXIT, MSG_RESIZE, MSG_SIGNAL, MSG_STDIN, ShellSession } from "../src/shell";

class MockSocket {
  readonly sent: Buffer[] = [];
  readyState = 1;
  close(): void {
    this.readyState = 3;
  }
  send(data: Buffer, callback: (error?: Error) => void): void {
    this.sent.push(data);
    callback();
  }
}

describe("ShellSession", () => {
  it("builds stdin frame", async () => {
    const session = new ShellSession("ws://localhost");
    const socket = new MockSocket();
    (session as unknown as { "#socket"?: MockSocket | null; _socket?: MockSocket | null })._socket = socket;
    Object.defineProperty(session, "ensureConnected", {
      value: () => socket,
      configurable: true,
    });

    await session.send("hello");

    expect(socket.sent).toHaveLength(1);
    expect(socket.sent[0]).toEqual(Buffer.concat([Buffer.from([MSG_STDIN]), Buffer.from("hello")]));
  });

  it("builds resize and signal frames", async () => {
    const session = new ShellSession("ws://localhost");
    const socket = new MockSocket();
    Object.defineProperty(session, "ensureConnected", {
      value: () => socket,
      configurable: true,
    });

    await session.resize(120, 40);
    await session.sendSignal(15);

    expect(socket.sent[0][0]).toBe(MSG_RESIZE);
    expect(socket.sent[0].readUInt16BE(1)).toBe(120);
    expect(socket.sent[0].readUInt16BE(3)).toBe(40);
    expect(socket.sent[1]).toEqual(Buffer.from([MSG_SIGNAL, 15]));
  });

  it("tracks exit code", async () => {
    const session = new ShellSession("ws://localhost");
    session.pushMessage(
      Buffer.concat([Buffer.from([MSG_EXIT]), Buffer.alloc(4)]),
    );

    const frame = await session.recv();
    expect(frame.type).toBe(MSG_EXIT);
    expect(session.exitCode).toBe(0);
  });

  it("throws when not connected", async () => {
    const session = new ShellSession("ws://localhost");
    await expect(session.send("hello")).rejects.toThrow("not connected");
  });
});
