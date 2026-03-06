package main

import (
	"testing"
)

func TestLabelsToMap(t *testing.T) {
	labels := []string{"self-hosted", "firecracker", "Linux"}
	m := labelsToMap(labels)

	if len(m) != 3 {
		t.Errorf("Expected 3 entries, got %d", len(m))
	}
	for _, label := range labels {
		if m[label] != "true" {
			t.Errorf("Expected %q -> \"true\", got %q", label, m[label])
		}
	}
}

func TestParseLabels(t *testing.T) {
	input := []byte(`["self-hosted","firecracker","Linux"]`)
	labels := parseLabels(input)

	if len(labels) != 3 {
		t.Errorf("Expected 3 labels, got %d", len(labels))
	}

	// Invalid JSON should return empty
	empty := parseLabels([]byte(`not-json`))
	if len(empty) != 0 {
		t.Errorf("Expected 0 labels for invalid JSON, got %d", len(empty))
	}
}
