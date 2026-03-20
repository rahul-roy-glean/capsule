package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func httpAddressWithScheme(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

// HandleCheckpointRunner creates a non-destructive checkpoint on the host and
// persists the durable session head in Postgres.
func (s *ControlPlaneServer) HandleCheckpointRunner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RunnerID string `json:"runner_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.RunnerID == "" {
		http.Error(w, "runner_id is required", http.StatusBadRequest)
		return
	}

	runner, err := s.hostRegistry.GetRunner(req.RunnerID)
	if err != nil {
		http.Error(w, "runner not found", http.StatusNotFound)
		return
	}
	host, err := s.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		http.Error(w, "host not found", http.StatusInternalServerError)
		return
	}

	cpCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	targetURL := strings.TrimRight(httpAddressWithScheme(host.HTTPAddress), "/") + "/api/v1/runners/" + req.RunnerID + "/checkpoint"
	upstreamReq, err := http.NewRequestWithContext(cpCtx, http.MethodPost, targetURL, nil)
	if err != nil {
		http.Error(w, "failed to create checkpoint request", http.StatusInternalServerError)
		return
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(upstreamReq)
	if err != nil {
		http.Error(w, "checkpoint failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var body struct {
		SessionID         string `json:"session_id"`
		Layer             int    `json:"layer"`
		Generation        int    `json:"generation"`
		SnapshotSizeBytes int64  `json:"snapshot_size_bytes"`
		ManifestPath      string `json:"manifest_path"`
		Running           bool   `json:"running"`
		Error             string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		http.Error(w, "failed to decode checkpoint response", http.StatusBadGateway)
		return
	}
	if resp.StatusCode >= 400 || body.Error != "" {
		errMsg := body.Error
		if errMsg == "" {
			errMsg = fmt.Sprintf("host checkpoint returned %d", resp.StatusCode)
		}
		http.Error(w, errMsg, http.StatusBadGateway)
		return
	}

	generation := body.Generation
	if generation == 0 {
		generation = body.Layer + 1
	}
	status := "active_checkpointed"
	if !body.Running {
		status = "suspended"
	}
	now := time.Now()
	headRec := SessionHeadRecord{
		SessionID:           body.SessionID,
		WorkloadKey:         runner.WorkloadKey,
		CurrentHostID:       host.ID,
		CurrentRunnerID:     req.RunnerID,
		Status:              status,
		LatestGeneration:    generation,
		LatestManifestPath:  body.ManifestPath,
		RunnerTTLSeconds:    runner.RunnerTTLSeconds,
		AutoPause:           runner.AutoPause,
		NetworkPolicyPreset: runner.NetworkPolicyPreset,
		NetworkPolicyJSON:   runner.NetworkPolicyJSON,
		LastCheckpointedAt:  now,
		LastActivityAt:      now,
	}
	if err := upsertSessionHead(cpCtx, s.scheduler.db, headRec); err != nil {
		http.Error(w, "failed to persist session head: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertSessionCheckpoint(cpCtx, s.scheduler.db, SessionCheckpointRecord{
		SessionID:         body.SessionID,
		Generation:        generation,
		ManifestPath:      body.ManifestPath,
		CheckpointKind:    "manual",
		TriggerSource:     "api",
		HostID:            host.ID,
		RunnerID:          req.RunnerID,
		SnapshotSizeBytes: body.SnapshotSizeBytes,
		CreatedAt:         now,
	}); err != nil {
		http.Error(w, "failed to persist session checkpoint: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success":             true,
		"session_id":          body.SessionID,
		"layer":               body.Layer,
		"generation":          generation,
		"manifest_path":       body.ManifestPath,
		"snapshot_size_bytes": body.SnapshotSizeBytes,
		"running":             body.Running,
	})
}

// HandleHostCheckpointUpdate persists a checkpoint produced autonomously on a host.
func (s *ControlPlaneServer) HandleHostCheckpointUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID                    string    `json:"session_id"`
		RunnerID                     string    `json:"runner_id"`
		WorkloadKey                  string    `json:"workload_key"`
		HostID                       string    `json:"host_id"`
		Generation                   int       `json:"generation"`
		ManifestPath                 string    `json:"manifest_path"`
		SnapshotSizeBytes            int64     `json:"snapshot_size_bytes"`
		Running                      bool      `json:"running"`
		CheckpointKind               string    `json:"checkpoint_kind"`
		TriggerSource                string    `json:"trigger_source"`
		RunnerTTLSeconds             int       `json:"runner_ttl_seconds"`
		AutoPause                    bool      `json:"auto_pause"`
		NetworkPolicyPreset          string    `json:"network_policy_preset"`
		NetworkPolicyJSON            string    `json:"network_policy_json"`
		CheckpointIntervalSeconds    int       `json:"checkpoint_interval_seconds"`
		CheckpointQuietWindowSeconds int       `json:"checkpoint_quiet_window_seconds"`
		LastActivityAt               time.Time `json:"last_activity_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || req.RunnerID == "" || req.WorkloadKey == "" || req.HostID == "" || req.Generation == 0 || req.ManifestPath == "" {
		http.Error(w, "missing required checkpoint fields", http.StatusBadRequest)
		return
	}
	if req.CheckpointKind == "" {
		req.CheckpointKind = "periodic"
	}
	if req.TriggerSource == "" {
		req.TriggerSource = "host_loop"
	}
	status := "active_checkpointed"
	if !req.Running {
		status = "suspended"
	}
	now := time.Now()
	headRec := SessionHeadRecord{
		SessionID:                    req.SessionID,
		WorkloadKey:                  req.WorkloadKey,
		CurrentHostID:                req.HostID,
		CurrentRunnerID:              req.RunnerID,
		Status:                       status,
		LatestGeneration:             req.Generation,
		LatestManifestPath:           req.ManifestPath,
		RunnerTTLSeconds:             req.RunnerTTLSeconds,
		AutoPause:                    req.AutoPause,
		NetworkPolicyPreset:          req.NetworkPolicyPreset,
		NetworkPolicyJSON:            req.NetworkPolicyJSON,
		CheckpointIntervalSeconds:    req.CheckpointIntervalSeconds,
		CheckpointQuietWindowSeconds: req.CheckpointQuietWindowSeconds,
		LastCheckpointedAt:           now,
		LastActivityAt:               req.LastActivityAt,
	}
	if err := upsertSessionHead(r.Context(), s.scheduler.db, headRec); err != nil {
		http.Error(w, "failed to upsert session head: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertSessionCheckpoint(r.Context(), s.scheduler.db, SessionCheckpointRecord{
		SessionID:         req.SessionID,
		Generation:        req.Generation,
		ManifestPath:      req.ManifestPath,
		CheckpointKind:    req.CheckpointKind,
		TriggerSource:     req.TriggerSource,
		HostID:            req.HostID,
		RunnerID:          req.RunnerID,
		SnapshotSizeBytes: req.SnapshotSizeBytes,
		CreatedAt:         now,
	}); err != nil {
		http.Error(w, "failed to insert session checkpoint: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": true})
}
