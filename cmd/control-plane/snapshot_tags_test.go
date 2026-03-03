package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSnapshotTag_JSONRoundTrip(t *testing.T) {
	tag := SnapshotTag{
		Tag:         "stable",
		WorkloadKey: "wk123",
		Version:     "v2.3.1",
		Description: "production release",
		CreatedAt:   time.Now().Truncate(time.Second),
	}

	data, err := json.Marshal(tag)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SnapshotTag
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Tag != "stable" {
		t.Errorf("Tag = %q, want %q", decoded.Tag, "stable")
	}
	if decoded.WorkloadKey != "wk123" {
		t.Errorf("WorkloadKey = %q, want %q", decoded.WorkloadKey, "wk123")
	}
	if decoded.Version != "v2.3.1" {
		t.Errorf("Version = %q, want %q", decoded.Version, "v2.3.1")
	}
	if decoded.Description != "production release" {
		t.Errorf("Description = %q, want %q", decoded.Description, "production release")
	}
}

func TestSnapshotTag_EmptyDescription(t *testing.T) {
	tag := SnapshotTag{
		Tag:         "canary",
		WorkloadKey: "wk456",
		Version:     "v1.0.0",
	}

	data, err := json.Marshal(tag)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SnapshotTag
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Description != "" {
		t.Errorf("Description should be empty, got %q", decoded.Description)
	}
}

func TestSnapshotTag_TagNameVariants(t *testing.T) {
	// Verify various tag names that should be valid
	names := []string{"stable", "canary", "v1.2.3", "production", "staging-v2", "my_tag"}

	for _, name := range names {
		tag := SnapshotTag{
			Tag:         name,
			WorkloadKey: "wk",
			Version:     "v1",
		}

		data, err := json.Marshal(tag)
		if err != nil {
			t.Fatalf("Marshal failed for tag %q: %v", name, err)
		}

		var decoded SnapshotTag
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Unmarshal failed for tag %q: %v", name, err)
		}

		if decoded.Tag != name {
			t.Errorf("Tag = %q, want %q", decoded.Tag, name)
		}
	}
}
