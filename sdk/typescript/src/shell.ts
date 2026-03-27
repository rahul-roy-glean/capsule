import WebSocket, { type RawData } from "ws";

export const MSG_STDIN = 0x00;
export const MSG_STDOUT = 0x01;
export const MSG_RESIZE = 0x02;
export const MSG_EXIT = 0x03;
export const MSG_SIGNAL = 0x04;

export interface ShellConnectOptions {
  reconnectUrlFactory?: () => Promise<string> | string;
  connectTimeoutMs?: number;
}

export class ShellSession {
  #url: string;
  #reconnectUrlFactory?: () => Promise<string> | string;
  #connectTimeoutMs?: number;
  #socket: WebSocket | null = null;
  #messages: Buffer[] = [];
  #messageWaiters: Array<(value: Buffer) => void> = [];
  #closeWaiters: Array<(error: Error) => void> = [];
  #exitCode: number | null = null;

  public constructor(url: string, options: ShellConnectOptions = {}) {
    this.#url = url;
    this.#reconnectUrlFactory = options.reconnectUrlFactory;
    this.#connectTimeoutMs = options.connectTimeoutMs;
  }

  public get url(): string {
    return this.#url;
  }

  public get exitCode(): number | null {
    return this.#exitCode;
  }

  public pushMessage(message: Buffer): void {
    const waiter = this.#messageWaiters.shift();
    if (waiter) {
      waiter(message);
      this.#closeWaiters.shift();
      return;
    }
    this.#messages.push(message);
  }

  public async connect(): Promise<void> {
    try {
      this.#socket = await this.open(this.#url);
    } catch (error) {
      if (!this.#reconnectUrlFactory) {
        throw error;
      }
      this.#url = await this.#reconnectUrlFactory();
      this.#socket = await this.open(this.#url);
    }
  }

  public async close(): Promise<void> {
    const socket = this.#socket;
    if (!socket) {
      return;
    }
    await new Promise<void>((resolve) => {
      socket.once("close", () => resolve());
      socket.close();
    });
    this.#socket = null;
  }

  public async send(data: Buffer | Uint8Array | string): Promise<void> {
    const payload = typeof data === "string" ? Buffer.from(data) : Buffer.from(data);
    await this.sendFrame(Buffer.concat([Buffer.from([MSG_STDIN]), payload]));
  }

  public async recv(timeoutMs?: number): Promise<{ type: number; payload: Buffer }> {
    const message = await this.recvMessage(timeoutMs);
    if (message.byteLength === 0) {
      return { type: MSG_STDOUT, payload: Buffer.alloc(0) };
    }

    const type = message[0] ?? MSG_STDOUT;
    const payload = message.subarray(1);
    if (type === MSG_EXIT && payload.byteLength >= 4) {
      this.#exitCode = payload.readInt32BE(0);
    }
    return { type, payload };
  }

  public async recvStdout(timeoutMs?: number): Promise<Buffer> {
    while (true) {
      const frame = await this.recv(timeoutMs);
      if (frame.type === MSG_STDOUT) {
        return frame.payload;
      }
      if (frame.type === MSG_EXIT) {
        return Buffer.alloc(0);
      }
    }
  }

  public async resize(cols: number, rows: number): Promise<void> {
    const frame = Buffer.alloc(5);
    frame[0] = MSG_RESIZE;
    frame.writeUInt16BE(cols, 1);
    frame.writeUInt16BE(rows, 3);
    await this.sendFrame(frame);
  }

  public async sendSignal(signal: number): Promise<void> {
    await this.sendFrame(Buffer.from([MSG_SIGNAL, signal]));
  }

  public async waitExit(timeoutMs?: number): Promise<number> {
    while (this.#exitCode === null) {
      const frame = await this.recv(timeoutMs);
      if (frame.type === MSG_EXIT && frame.payload.byteLength >= 4) {
        this.#exitCode = frame.payload.readInt32BE(0);
      }
    }
    return this.#exitCode;
  }

  public async using<T>(fn: (shell: ShellSession) => Promise<T>): Promise<T> {
    await this.connect();
    try {
      return await fn(this);
    } finally {
      await this.close();
    }
  }

  private async sendFrame(frame: Buffer): Promise<void> {
    const socket = this.ensureConnected();
    await new Promise<void>((resolve, reject) => {
      socket.send(frame, (error) => {
        if (error) {
          reject(error);
          return;
        }
        resolve();
      });
    });
  }

  private async recvMessage(timeoutMs?: number): Promise<Buffer> {
    if (this.#messages.length > 0) {
      return this.#messages.shift()!;
    }

    const pending = new Promise<Buffer>((resolve, reject) => {
      this.#messageWaiters.push(resolve);
      this.#closeWaiters.push(reject);
    });

    if (timeoutMs == null) {
      return pending;
    }

    return Promise.race([
      pending,
      new Promise<Buffer>((_, reject) => {
        setTimeout(() => reject(new Error("Timed out waiting for PTY output.")), timeoutMs);
      }),
    ]);
  }

  private ensureConnected(): WebSocket {
    if (!this.#socket || this.#socket.readyState !== WebSocket.OPEN) {
      throw new Error("ShellSession is not connected. Call connect() first.");
    }
    return this.#socket;
  }

  private async open(url: string): Promise<WebSocket> {
    return new Promise<WebSocket>((resolve, reject) => {
      const socket = new WebSocket(url, { handshakeTimeout: this.#connectTimeoutMs });

      const cleanup = (): void => {
        socket.removeAllListeners("open");
        socket.removeAllListeners("error");
      };

      socket.once("open", () => {
        cleanup();
        socket.on("message", (data: RawData) => {
          const message = rawDataToBuffer(data);
          const waiter = this.#messageWaiters.shift();
          if (waiter) {
            waiter(message);
            this.#closeWaiters.shift();
          } else {
            this.#messages.push(message);
          }
        });
        socket.on("close", () => {
          this.#socket = null;
          const error = new Error("PTY connection closed.");
          for (const waiter of this.#closeWaiters.splice(0)) {
            waiter(error);
          }
          this.#messageWaiters.length = 0;
        });
        resolve(socket);
      });
      socket.once("error", (error) => {
        cleanup();
        reject(error);
      });
    });
  }
}

function rawDataToBuffer(data: RawData): Buffer {
  if (Buffer.isBuffer(data)) {
    return data;
  }
  if (Array.isArray(data)) {
    return Buffer.concat(data.map((value) => Buffer.from(value)));
  }
  return Buffer.from(data);
}
