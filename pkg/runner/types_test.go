package runner

import (
	"encoding/json"
	"testing"
)

func TestMMDSData_JobID(t *testing.T) {
	var data MMDSData
	data.Latest.Meta.RunnerID = "runner-1"
	data.Latest.Meta.HostID = "host-1"
	data.Latest.Meta.JobID = "gh-12345"
	data.Latest.Meta.Environment = "test"
	data.Latest.Job.Repo = "org/repo"
	data.Latest.Snapshot.Version = "v1"

	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("Failed to marshal MMDSData: %v", err)
	}

	var decoded MMDSData
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal MMDSData: %v", err)
	}

	if decoded.Latest.Meta.JobID != "gh-12345" {
		t.Errorf("JobID = %q, want %q", decoded.Latest.Meta.JobID, "gh-12345")
	}
}

func TestMMDSData_BackwardsCompatible(t *testing.T) {
	// Old MMDS data without JobID should still unmarshal
	oldJSON := `{"latest":{"meta":{"runner_id":"r1","host_id":"h1","environment":"prod"},"network":{"ip":"10.0.0.1","gateway":"10.0.0.254","netmask":"255.255.255.0","dns":"8.8.8.8","interface":"eth0","mac":"aa:bb:cc:dd:ee:ff"},"job":{"repo":"org/repo","branch":"main","commit":"abc123"},"snapshot":{"version":"v1"}}}`

	var data MMDSData
	if err := json.Unmarshal([]byte(oldJSON), &data); err != nil {
		t.Fatalf("Failed to unmarshal old MMDS data: %v", err)
	}

	if data.Latest.Meta.RunnerID != "r1" {
		t.Errorf("RunnerID = %q, want %q", data.Latest.Meta.RunnerID, "r1")
	}
	if data.Latest.Meta.JobID != "" {
		t.Errorf("JobID should be empty for old data, got %q", data.Latest.Meta.JobID)
	}
}

func TestAllocateRequest_ChunkKey(t *testing.T) {
	req := AllocateRequest{
		RequestID: "req-1",
		Repo:      "https://github.com/org/repo",
		ChunkKey:  "abc1234567890abc",
		Branch:    "main",
	}

	if req.ChunkKey != "abc1234567890abc" {
		t.Errorf("ChunkKey = %q, want %q", req.ChunkKey, "abc1234567890abc")
	}
}

func TestRunnerKey_Match(t *testing.T) {
	tests := []struct {
		name string
		a    *RunnerKey
		b    *RunnerKey
		want bool
	}{
		{
			"same version matches",
			&RunnerKey{SnapshotVersion: "v1", Platform: "linux/amd64"},
			&RunnerKey{SnapshotVersion: "v1", Platform: "linux/amd64"},
			true,
		},
		{
			"different version no match",
			&RunnerKey{SnapshotVersion: "v1"},
			&RunnerKey{SnapshotVersion: "v2"},
			false,
		},
		{
			"same repo matches",
			&RunnerKey{SnapshotVersion: "v1", GitHubRepo: "org/repo"},
			&RunnerKey{SnapshotVersion: "v1", GitHubRepo: "org/repo"},
			true,
		},
		{
			"different repo no match",
			&RunnerKey{SnapshotVersion: "v1", GitHubRepo: "org/repo-a"},
			&RunnerKey{SnapshotVersion: "v1", GitHubRepo: "org/repo-b"},
			false,
		},
		{
			"empty repo matches anything",
			&RunnerKey{SnapshotVersion: "v1", GitHubRepo: ""},
			&RunnerKey{SnapshotVersion: "v1", GitHubRepo: "org/repo"},
			true,
		},
		{
			"nil keys match",
			nil,
			nil,
			true,
		},
		{
			"nil vs non-nil no match",
			nil,
			&RunnerKey{SnapshotVersion: "v1"},
			false,
		},
	}

	pool := &Pool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pool.keysMatch(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("keysMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunnerStates(t *testing.T) {
	// Verify all expected states are defined
	states := []State{
		StateCold, StateBooting, StateInitializing, StateIdle,
		StateBusy, StateDraining, StateQuarantined, StateRetiring,
		StateTerminated, StatePaused,
	}

	seen := make(map[State]bool)
	for _, s := range states {
		if seen[s] {
			t.Errorf("Duplicate state: %s", s)
		}
		seen[s] = true
		if s == "" {
			t.Error("State should not be empty")
		}
	}
}
