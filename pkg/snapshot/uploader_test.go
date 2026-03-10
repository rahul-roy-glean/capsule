package snapshot

import (
	"encoding/json"
	"testing"
)

func TestPointerJSONRoundtrip(t *testing.T) {
	version := "v20260216-044453-master"

	// Simulate what UpdateCurrentPointer writes
	pointer := struct {
		Version string `json:"version"`
	}{Version: version}

	data, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("Failed to marshal pointer: %v", err)
	}

	// Simulate what resolveCurrentPointer reads
	var decoded struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal pointer: %v", err)
	}

	if decoded.Version != version {
		t.Errorf("Pointer roundtrip: got %q, want %q", decoded.Version, version)
	}

	// Verify JSON format
	expected := `{"version":"v20260216-044453-master"}`
	if string(data) != expected {
		t.Errorf("Pointer JSON: got %s, want %s", string(data), expected)
	}
}

func TestPointerJSONParsing(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		version string
		wantErr bool
	}{
		{"compact", `{"version":"v20260216-044453-master"}`, "v20260216-044453-master", false},
		{"with_spaces", `{ "version": "v20260216-044453-master" }`, "v20260216-044453-master", false},
		{"empty_version", `{"version":""}`, "", false},
		{"extra_fields", `{"version":"v1","extra":"ignored"}`, "v1", false},
		{"invalid_json", `not json`, "", true},
		{"missing_version_field", `{"other":"field"}`, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var pointer struct {
				Version string `json:"version"`
			}
			err := json.Unmarshal([]byte(tc.input), &pointer)
			if tc.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if pointer.Version != tc.version {
				t.Errorf("Version: got %q, want %q", pointer.Version, tc.version)
			}
		})
	}
}
