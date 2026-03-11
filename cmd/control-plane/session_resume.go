package main

import (
	"encoding/json"
	"fmt"
	"time"
)

const sessionResumeHostFreshnessWindow = 60 * time.Second

// selectOriginalSessionResumeHost returns the original host for a suspended
// session only when the host is still ready and heartbeating recently enough to
// be considered allocatable. Callers can then decide whether to fail closed or
// fall back to a different host when portable restore metadata is available.
func selectOriginalSessionResumeHost(hr *HostRegistry, originalHostID string, now time.Time) (*Host, string) {
	if hr == nil {
		return nil, "host registry unavailable"
	}

	origHost, err := hr.GetHost(originalHostID)
	if err != nil {
		return nil, "original host not found"
	}
	if origHost.Status != "ready" {
		return nil, fmt.Sprintf("original host status=%s", origHost.Status)
	}
	if now.Sub(origHost.LastHeartbeat) >= sessionResumeHostFreshnessWindow {
		return nil, "original host heartbeat stale"
	}

	return origHost, ""
}

// sessionRestoreMetadataSupportsCrossHost reports whether the stored restore
// metadata is portable enough to attempt a cross-host resume. Today that means
// the session has a GCS-backed manifest the destination host can use to rebuild
// restore state without a local metadata.json or local layer files.
func sessionRestoreMetadataSupportsCrossHost(metadataJSON string) bool {
	if metadataJSON == "" {
		return false
	}
	var meta struct {
		GCSManifestPath string `json:"gcs_manifest_path"`
	}
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return false
	}
	return meta.GCSManifestPath != ""
}
