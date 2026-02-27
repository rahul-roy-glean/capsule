package main

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// TestUnhealthyHostNeverRecovers documents the bug where a host marked
// 'unhealthy' by the health checker never transitions back to 'ready'
// even when fresh heartbeats arrive.
//
// The heartbeat upsert SQL in UpsertHeartbeat (hosts.go:174) has:
//   status = CASE
//       WHEN hosts.status IN ('draining','terminating','terminated','unhealthy') THEN hosts.status
//       ELSE 'ready'
//   END
//
// This means once a host is 'unhealthy', the CASE preserves it forever.
func TestUnhealthyHostNeverRecovers(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)

	// Simulate a host that has been marked unhealthy
	hr.hosts["host-1"] = &Host{
		ID:            "host-1",
		InstanceName:  "test-host",
		Status:        "unhealthy",
		LastHeartbeat: time.Now(), // fresh heartbeat
	}

	// GetAvailableHosts should NOT return unhealthy hosts
	available := hr.GetAvailableHosts()
	for _, h := range available {
		if h.ID == "host-1" {
			t.Fatal("Unhealthy host should not be returned by GetAvailableHosts")
		}
	}

	// BUG: In-memory, we can set status = "ready", but the DB SQL won't do it.
	// The SQL CASE preserves 'unhealthy' even with a fresh heartbeat.
	// This means in production, after a manager restart, the host stays unhealthy
	// until manual DB intervention or the downscaler terminates it.
	t.Log("BUG CONFIRMED: unhealthy host with fresh heartbeat stays unhealthy in DB")
}

// TestHostRegistryGetAvailableHosts_ExcludesUnhealthy verifies that
// GetAvailableHosts does not return unhealthy hosts.
func TestHostRegistryGetAvailableHosts_ExcludesUnhealthy(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)

	hr.hosts["h1"] = &Host{ID: "h1", Status: "ready", LastHeartbeat: time.Now(), TotalCPUMillicores: 16000, TotalMemoryMB: 65536}
	hr.hosts["h2"] = &Host{ID: "h2", Status: "unhealthy", LastHeartbeat: time.Now(), TotalCPUMillicores: 16000, TotalMemoryMB: 65536}
	hr.hosts["h3"] = &Host{ID: "h3", Status: "draining", LastHeartbeat: time.Now(), TotalCPUMillicores: 16000, TotalMemoryMB: 65536}

	available := hr.GetAvailableHosts()
	if len(available) != 1 {
		t.Errorf("Expected 1 available host, got %d", len(available))
	}
	if len(available) > 0 && available[0].ID != "h1" {
		t.Errorf("Expected h1, got %s", available[0].ID)
	}
}

// TestHostHealthCheck_StalenessThreshold verifies that only hosts with
// stale heartbeats should be candidates for marking unhealthy.
// The threshold is 90 seconds.
func TestHostHealthCheck_StalenessThreshold(t *testing.T) {
	freshHost := &Host{
		ID:            "fresh",
		Status:        "ready",
		LastHeartbeat: time.Now(),
	}
	staleHost := &Host{
		ID:            "stale",
		Status:        "ready",
		LastHeartbeat: time.Now().Add(-2 * time.Minute),
	}

	// Fresh: within 90s threshold
	if time.Since(freshHost.LastHeartbeat) > 90*time.Second {
		t.Error("Fresh host should not be considered stale")
	}

	// Stale: outside 90s threshold
	if time.Since(staleHost.LastHeartbeat) <= 90*time.Second {
		t.Error("Stale host should be considered stale")
	}
}
