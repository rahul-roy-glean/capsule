import type {
  FileListResult,
  FileMkdirResult,
  FileReadResult,
  FileRemoveResult,
  FileStatResult,
  FileUploadResult,
  FileWriteResult,
} from "./models/file";
import type { ExecEvent, ExecResult, PauseResult } from "./models/runner";
import type { Runners } from "./resources/runners";
import { ShellSession } from "./shell";

export interface ExecCallbacks {
  onStdout?: (event: ExecEvent) => void | Promise<void>;
  onStderr?: (event: ExecEvent) => void | Promise<void>;
  onExit?: (code: number) => void | Promise<void>;
}

export interface ExecCommandOptions extends ExecCallbacks {
  env?: Record<string, string>;
  workingDir?: string;
  timeoutSeconds?: number;
}

async function maybeAwait(value: void | Promise<void>): Promise<void> {
  await value;
}

export class RunnerSession {
  #runners: Runners;
  #released = false;

  readonly runnerId: string;
  sessionId?: string;
  requestId?: string;

  public constructor(
    runners: Runners,
    runnerId: string,
    options: {
      hostAddress?: string;
      sessionId?: string;
      requestId?: string;
    } = {},
  ) {
    this.#runners = runners;
    this.runnerId = runnerId;
    this.sessionId = options.sessionId;
    this.requestId = options.requestId;
    if (options.hostAddress) {
      this.#runners.setHostCache(runnerId, options.hostAddress);
    }
  }

  public async waitReady(options: { timeout?: number; pollInterval?: number } = {}): Promise<void> {
    await this.#runners.waitReady(this.runnerId, options);
  }

  public async pause(options: { syncFs?: boolean } = {}): Promise<PauseResult> {
    const result = await this.#runners.pause(this.runnerId, options);
    if (result.sessionId) {
      this.sessionId = result.sessionId;
    }
    return result;
  }

  public async release(): Promise<boolean> {
    if (this.#released) {
      return true;
    }
    const ok = await this.#runners.release(this.runnerId);
    if (ok) {
      this.#released = true;
    }
    return ok;
  }

  public exec(...command: string[]): AsyncIterable<ExecEvent> {
    return this.#runners.exec(this.runnerId, command, {});
  }

  public async execCollect(...command: string[]): Promise<ExecResult> {
    const stdoutParts: string[] = [];
    const stderrParts: string[] = [];
    let exitCode = -1;
    const started = Date.now();

    for await (const event of this.#runners.exec(this.runnerId, command, {})) {
      if (event.type === "stdout" && event.data) {
        stdoutParts.push(event.data);
      } else if (event.type === "stderr" && event.data) {
        stderrParts.push(event.data);
      } else if (event.type === "exit" && typeof event.code === "number") {
        exitCode = event.code;
      }
    }

    return {
      stdout: stdoutParts.join(""),
      stderr: stderrParts.join(""),
      exitCode,
      durationMs: Date.now() - started,
    };
  }

  public shell(options: { command?: string; cols?: number; rows?: number } = {}): ShellSession {
    return this.#runners.shell(this.runnerId, options);
  }

  public download(path: string): Promise<Uint8Array> {
    return this.#runners.fileDownload(this.runnerId, path);
  }

  public upload(
    path: string,
    data: Uint8Array | ArrayBuffer | string,
    options: { mode?: string; perm?: string } = {},
  ): Promise<FileUploadResult> {
    const payload =
      typeof data === "string" ? new TextEncoder().encode(data) : data instanceof Uint8Array ? data : new Uint8Array(data);
    return this.#runners.fileUpload(this.runnerId, path, payload, options);
  }

  public readFile(path: string, options: { offset?: number; limit?: number } = {}): Promise<FileReadResult> {
    return this.#runners.fileRead(this.runnerId, path, options);
  }

  public async readText(path: string, options: { offset?: number; limit?: number } = {}): Promise<string> {
    const result = await this.readFile(path, options);
    return result.content ?? "";
  }

  public writeFile(path: string, content: string, options: { mode?: string } = {}): Promise<FileWriteResult> {
    return this.#runners.fileWrite(this.runnerId, path, content, options);
  }

  public writeText(path: string, content: string, options: { mode?: string } = {}): Promise<FileWriteResult> {
    return this.writeFile(path, content, options);
  }

  public listFiles(path: string, options: { recursive?: boolean } = {}): Promise<FileListResult> {
    return this.#runners.fileList(this.runnerId, path, options);
  }

  public statFile(path: string): Promise<FileStatResult> {
    return this.#runners.fileStat(this.runnerId, path);
  }

  public remove(path: string, options: { recursive?: boolean } = {}): Promise<FileRemoveResult> {
    return this.#runners.fileRemove(this.runnerId, path, options);
  }

  public mkdir(path: string): Promise<FileMkdirResult> {
    return this.#runners.fileMkdir(this.runnerId, path);
  }

  public quarantine(options: { reason?: string; blockEgress?: boolean; pauseVm?: boolean } = {}): Promise<Record<string, unknown>> {
    return this.#runners.quarantine(this.runnerId, options);
  }

  public unquarantine(options: { unblockEgress?: boolean; resumeVm?: boolean } = {}): Promise<Record<string, unknown>> {
    return this.#runners.unquarantine(this.runnerId, options);
  }
}
