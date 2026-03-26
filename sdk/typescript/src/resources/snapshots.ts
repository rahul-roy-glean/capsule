import type { HttpClient } from "../http";
import { parseSnapshotListResponse, type Snapshot } from "../models/snapshot";

export class Snapshots {
  private readonly http: HttpClient;

  public constructor(http: HttpClient) {
    this.http = http;
  }

  public async list(): Promise<Snapshot[]> {
    const data = await this.http.get("/api/v1/snapshots");
    return parseSnapshotListResponse(data).snapshots;
  }
}
