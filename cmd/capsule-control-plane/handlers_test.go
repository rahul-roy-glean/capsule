package main

import (
	"testing"
	"time"
)

func TestBuildAndParseRunnersCursor(t *testing.T) {
	runnerID := "runner-abc-123"
	createdAt := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	// Build cursor
	cursor := buildRunnersCursor(runnerID, createdAt)
	if cursor == "" {
		t.Fatal("buildRunnersCursor returned empty string")
	}

	// Parse cursor
	parsedTimestamp, parsedRunnerID, err := parseRunnersCursor(cursor)
	if err != nil {
		t.Fatalf("parseRunnersCursor failed: %v", err)
	}

	if parsedRunnerID != runnerID {
		t.Errorf("runner_id mismatch: expected %s, got %s", runnerID, parsedRunnerID)
	}

	if parsedTimestamp != createdAt.Unix() {
		t.Errorf("timestamp mismatch: expected %d, got %d", createdAt.Unix(), parsedTimestamp)
	}
}

func TestParseRunnersCursor_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		cursor string
	}{
		{"empty", ""},
		{"invalid base64", "not-base64!@#$"},
		{"invalid format", "bm90LWEtdmFsaWQtY3Vyc29y"}, // "not-a-valid-cursor" encoded
		{"missing separator", "MTIzNDU2Nzg5"}, // "123456789" encoded (no underscore)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseRunnersCursor(tt.cursor)
			if err == nil {
				t.Errorf("expected parseRunnersCursor to fail for %s", tt.name)
			}
		})
	}
}

func TestRunnerFilters(t *testing.T) {
	filters := RunnerFilters{
		Status:      "running",
		HostID:      "host-123",
		WorkloadKey: "wk-abc",
	}

	if filters.Status != "running" {
		t.Errorf("expected Status=running, got %s", filters.Status)
	}
	if filters.HostID != "host-123" {
		t.Errorf("expected HostID=host-123, got %s", filters.HostID)
	}
	if filters.WorkloadKey != "wk-abc" {
		t.Errorf("expected WorkloadKey=wk-abc, got %s", filters.WorkloadKey)
	}
}

func TestPaginationInfo(t *testing.T) {
	pagination := &PaginationInfo{
		NextCursor: "some-cursor",
		HasMore:    true,
		TotalCount: intPtr(150),
	}

	if pagination.NextCursor != "some-cursor" {
		t.Errorf("expected NextCursor=some-cursor, got %s", pagination.NextCursor)
	}
	if !pagination.HasMore {
		t.Error("expected HasMore=true")
	}
	if pagination.TotalCount == nil || *pagination.TotalCount != 150 {
		t.Errorf("expected TotalCount=150, got %v", pagination.TotalCount)
	}
}

func intPtr(i int) *int {
	return &i
}
