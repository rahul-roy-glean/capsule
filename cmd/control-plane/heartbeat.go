package main

import (
	"encoding/json"
	"io"
	"net/http"
)

type hostHeartbeatRequest struct {
	InstanceName    string `json:"instance_name"`
	Zone            string `json:"zone"`
	GRPCAddress     string `json:"grpc_address"`
	HTTPAddress     string `json:"http_address"`
	TotalSlots      int    `json:"total_slots"`
	UsedSlots       int    `json:"used_slots"`
	IdleRunners     int    `json:"idle_runners"`
	BusyRunners     int    `json:"busy_runners"`
	SnapshotVersion string `json:"snapshot_version"`
	Draining        bool   `json:"draining"`
	// LoadedManifests reports which chunk manifests are already loaded on this host
	// (chunk_key → version). Used by the control plane for cache-affinity scheduling.
	LoadedManifests map[string]string `json:"loaded_manifests,omitempty"`
}

type hostHeartbeatResponse struct {
	Acknowledged       bool   `json:"acknowledged"`
	ShouldDrain        bool   `json:"should_drain"`
	ShouldSyncSnapshot bool   `json:"should_sync_snapshot,omitempty"`
	SnapshotVersion    string `json:"snapshot_version,omitempty"`
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
		InstanceName:    req.InstanceName,
		Zone:            req.Zone,
		GRPCAddress:     req.GRPCAddress,
		HTTPAddress:     req.HTTPAddress,
		TotalSlots:      req.TotalSlots,
		UsedSlots:       req.UsedSlots,
		IdleRunners:     req.IdleRunners,
		BusyRunners:     req.BusyRunners,
		SnapshotVersion: req.SnapshotVersion,
		LoadedManifests: req.LoadedManifests,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, hostHeartbeatResponse{
			Acknowledged: false,
			Error:        err.Error(),
		})
		return
	}

	// Check if host needs a snapshot sync (legacy single-repo path)
	currentSnapshot := s.snapshotManager.GetCurrentVersion()
	shouldSync := currentSnapshot != "" && currentSnapshot != req.SnapshotVersion

	// Compute per-repo sync directives: only send repos whose desired version the
	// host doesn't already have loaded. This avoids re-downloading snapshot.mem on
	// every heartbeat and prevents every host from downloading every repo.
	var syncVersions map[string]string
	if s.snapshotManager.db != nil {
		desired, err := s.snapshotManager.GetDesiredVersions(r.Context(), host.ID)
		if err != nil {
			s.logger.WithError(err).Warn("Failed to get desired versions for heartbeat")
		} else {
			for chunkKey, ver := range desired {
				loaded, hasLoaded := req.LoadedManifests[chunkKey]
				if !hasLoaded || loaded != ver {
					if syncVersions == nil {
						syncVersions = make(map[string]string)
					}
					syncVersions[chunkKey] = ver
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, hostHeartbeatResponse{
		Acknowledged:       true,
		ShouldDrain:        shouldDrain,
		ShouldSyncSnapshot: shouldSync,
		SnapshotVersion:    currentSnapshot,
		SyncVersions:       syncVersions,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
