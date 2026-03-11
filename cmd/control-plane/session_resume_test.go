package main

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestSelectOriginalSessionResumeHost(t *testing.T) {
	now := time.Now()
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	tests := []struct {
		name       string
		hostID     string
		host       *Host
		wantHost   bool
		wantReason string
	}{
		{
			name:   "fresh ready host is reusable",
			hostID: "host-1",
			host: &Host{
				ID:            "host-1",
				Status:        "ready",
				LastHeartbeat: now.Add(-15 * time.Second),
			},
			wantHost: true,
		},
		{
			name:   "stale host is rejected",
			hostID: "host-2",
			host: &Host{
				ID:            "host-2",
				Status:        "ready",
				LastHeartbeat: now.Add(-2 * time.Minute),
			},
			wantReason: "original host heartbeat stale",
		},
		{
			name:   "draining host is rejected",
			hostID: "host-3",
			host: &Host{
				ID:            "host-3",
				Status:        "draining",
				LastHeartbeat: now.Add(-10 * time.Second),
			},
			wantReason: "original host status=draining",
		},
		{
			name:   "unhealthy host is rejected",
			hostID: "host-4",
			host: &Host{
				ID:            "host-4",
				Status:        "unhealthy",
				LastHeartbeat: now.Add(-10 * time.Second),
			},
			wantReason: "original host status=unhealthy",
		},
		{
			name:       "missing host is rejected",
			hostID:     "missing",
			wantReason: "original host not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hr := NewHostRegistry(nil, logger)
			if tt.host != nil {
				hr.hosts[tt.host.ID] = tt.host
			}

			gotHost, gotReason := selectOriginalSessionResumeHost(hr, tt.hostID, now)
			if tt.wantHost {
				if gotHost == nil {
					t.Fatalf("expected host, got nil with reason %q", gotReason)
				}
				if gotHost.ID != tt.hostID {
					t.Fatalf("selected host = %q, want %q", gotHost.ID, tt.hostID)
				}
				if gotReason != "" {
					t.Fatalf("expected empty reason, got %q", gotReason)
				}
				return
			}

			if gotHost != nil {
				t.Fatalf("expected no host, got %q", gotHost.ID)
			}
			if gotReason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, tt.wantReason)
			}
		})
	}
}

func TestSessionRestoreMetadataSupportsCrossHost(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		want     bool
	}{
		{
			name:     "empty metadata is not portable",
			metadata: "",
			want:     false,
		},
		{
			name:     "invalid json is not portable",
			metadata: "{not-json}",
			want:     false,
		},
		{
			name:     "missing gcs manifest is not portable",
			metadata: `{"session_id":"sess-1","workload_key":"wk"}`,
			want:     false,
		},
		{
			name:     "gcs manifest enables cross host resume",
			metadata: `{"session_id":"sess-1","workload_key":"wk","gcs_manifest_path":"v1/wk/runner_state/r1/snapshot_manifest.json"}`,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionRestoreMetadataSupportsCrossHost(tt.metadata); got != tt.want {
				t.Fatalf("sessionRestoreMetadataSupportsCrossHost() = %v, want %v", got, tt.want)
			}
		})
	}
}
