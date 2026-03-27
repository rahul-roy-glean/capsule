import { ConnectionConfig, type ConnectionOptions, type ResolvedConnectionConfig } from "./config";
import { HttpClient } from "./http";
import { RunnerConfigs } from "./runner-config";
import { LayeredConfigs } from "./resources/layered-configs";
import { Runners } from "./resources/runners";
import { Snapshots } from "./resources/snapshots";
import { Workloads } from "./resources/workloads";

export class CapsuleClient {
  readonly config: ResolvedConnectionConfig;
  readonly runners: Runners;
  readonly snapshots: Snapshots;
  readonly runnerConfigs: RunnerConfigs;
  readonly workloads: Workloads;

  private readonly http: HttpClient;
  private readonly layeredConfigs: LayeredConfigs;

  constructor(options: ConnectionOptions = {}) {
    this.config = ConnectionConfig.resolve(options);
    this.http = new HttpClient(this.config);
    this.layeredConfigs = new LayeredConfigs(this.http);
    this.runners = new Runners(this.http, this.layeredConfigs);
    this.snapshots = new Snapshots(this.http);
    this.runnerConfigs = new RunnerConfigs(this.layeredConfigs);
    this.workloads = new Workloads(this.layeredConfigs, this.runners);
  }

  async close(): Promise<void> {
    return Promise.resolve();
  }
}
