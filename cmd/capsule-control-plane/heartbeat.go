package main

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type hostHeartbeatRequest struct {
	InstanceName       string `json:"instance_name"`
	Zone               string `json:"zone"`
	GRPCAddress        string `json:"grpc_address"`
	HTTPAddress        string `json:"http_address"`
	IdleRunners        int    `json:"idle_runners"`
	BusyRunners        int    `json:"busy_runners"`
	SnapshotVersion    string `json:"snapshot_version"`
	Draining           bool   `json:"draining"`
	TotalCPUMillicores int    `json:"total_cpu_millicores"`
	UsedCPUMillicores  int    `json:"used_cpu_millicores"`
	TotalMemoryMB      int    `json:"total_memory_mb"`
	UsedMemoryMB       int    `json:"used_memory_mb"`
	// LoadedManifests reports which chunk manifests are already loaded on this host
	// (workload_key → version). Used by the control plane for cache-affinity scheduling.
	LoadedManifests map[string]string     `json:"loaded_manifests,omitempty"`
	Runners         []heartbeatRunnerInfo `json:"runners,omitempty"`
}

// heartbeatRunnerInfo mirrors runner.RunnerHeartbeatInfo for JSON deserialization.
type heartbeatRunnerInfo struct {
	RunnerID    string `json:"runner_id"`
	State       string `json:"state"`
	SessionID   string `json:"session_id,omitempty"`
	WorkloadKey string `json:"workload_key"`
	IdleSince   string `json:"idle_since,omitempty"` // RFC3339
}

type hostHeartbeatResponse struct {
	Acknowledged bool `json:"acknowledged"`
	ShouldDrain  bool `json:"should_drain"`
	// SyncVersions tells the host which repo manifests (and snapshot.mem files) to
	// pre-download. Only repos whose desired version differs from what the host has
	// loaded are included, so a host only downloads what it's missing.
	SyncVersions map[string]string `json:"sync_versions,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func (s *ControlPlaneServer) HandleHostHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req hostHeartbeatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.InstanceName == "" {
		http.Error(w, "missing instance_name", http.StatusBadRequest)
		return
	}

	host, shouldDrain, err := s.hostRegistry.UpsertHeartbeat(r.Context(), HostHeartbeat{
		InstanceName:       req.InstanceName,
		Zone:               req.Zone,
		GRPCAddress:        req.GRPCAddress,
		HTTPAddress:        req.HTTPAddress,
		IdleRunners:        req.IdleRunners,
		BusyRunners:        req.BusyRunners,
		SnapshotVersion:    req.SnapshotVersion,
		LoadedManifests:    req.LoadedManifests,
		TotalCPUMillicores: req.TotalCPUMillicores,
		UsedCPUMillicores:  req.UsedCPUMillicores,
		TotalMemoryMB:      req.TotalMemoryMB,
		UsedMemoryMB:       req.UsedMemoryMB,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, hostHeartbeatResponse{
			Acknowledged: false,
			Error:        err.Error(),
		})
		return
	}

	// Log host usage metrics for observability.
	logFields := map[string]any{
		"instance_name":        req.InstanceName,
		"status":               host.Status,
		"idle_runners":         req.IdleRunners,
		"busy_runners":         req.BusyRunners,
		"total_cpu_millicores": req.TotalCPUMillicores,
		"used_cpu_millicores":  req.UsedCPUMillicores,
		"total_memory_mb":      req.TotalMemoryMB,
		"used_memory_mb":       req.UsedMemoryMB,
	}
	if req.TotalCPUMillicores > 0 {
		logFields["cpu_util_pct"] = float64(req.UsedCPUMillicores) / float64(req.TotalCPUMillicores) * 100
	}
	if req.TotalMemoryMB > 0 {
		logFields["mem_util_pct"] = float64(req.UsedMemoryMB) / float64(req.TotalMemoryMB) * 100
	}
	s.logger.WithFields(logFields).Info("Host heartbeat")

	// If the host reported itself as draining (e.g. SIGTERM received), mark it
	// in the registry so the scheduler stops allocating to it.
	if req.Draining {
		if err := s.hostRegistry.SetHostStatusByInstanceName(r.Context(), req.InstanceName, "draining"); err != nil {
			s.logger.WithError(err).WithField("instance_name", req.InstanceName).Warn("Failed to mark host as draining from heartbeat")
		}
	}

	// Store per-runner info on the in-memory host for TTL enforcement.
	if len(req.Runners) > 0 {
		infos := make([]HostRunnerInfo, 0, len(req.Runners))
		for _, ri := range req.Runners {
			info := HostRunnerInfo{
				RunnerID:    ri.RunnerID,
				State:       ri.State,
				SessionID:   ri.SessionID,
				WorkloadKey: ri.WorkloadKey,
			}
			if ri.IdleSince != "" {
				if t, err := time.Parse(time.RFC3339, ri.IdleSince); err == nil {
					info.IdleSince = t
				}
			}
			infos = append(infos, info)
		}
		s.hostRegistry.mu.Lock()
		host.RunnerInfos = infos
		for _, info := range infos {
			runner := s.hostRegistry.runners[info.RunnerID]
			if runner == nil {
				runner = &Runner{
					ID:          info.RunnerID,
					HostID:      host.ID,
					Status:      info.State,
					SessionID:   info.SessionID,
					WorkloadKey: info.WorkloadKey,
				}
				s.hostRegistry.runners[info.RunnerID] = runner
				continue
			}
			runner.HostID = host.ID
			runner.Status = info.State
			runner.SessionID = info.SessionID
			runner.WorkloadKey = info.WorkloadKey
		}
		s.hostRegistry.mu.Unlock()
	} else {
		s.hostRegistry.mu.Lock()
		host.RunnerInfos = nil
		s.hostRegistry.mu.Unlock()
	}

	// Compute per-workload-key sync directives: only send workload keys whose
	// desired version the host doesn't already have loaded. This avoids
	// re-downloading snapshot.mem on every heartbeat.
	var syncVersions map[string]string
	if s.snapshotManager.db != nil {
		desired, err := s.snapshotManager.GetDesiredVersions(r.Context(), host.ID)
		if err != nil {
			s.logger.WithError(err).Warn("Failed to get desired versions for heartbeat")
		} else {
			for workloadKey, ver := range desired {
				loaded, hasLoaded := req.LoadedManifests[workloadKey]
				if !hasLoaded || loaded != ver {
					if syncVersions == nil {
						syncVersions = make(map[string]string)
					}
					syncVersions[workloadKey] = ver
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, hostHeartbeatResponse{
		Acknowledged: true,
		ShouldDrain:  shouldDrain,
		SyncVersions: syncVersions,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
