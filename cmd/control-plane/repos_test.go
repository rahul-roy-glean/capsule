package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleSnapshotConfigs_CreateMissingCommands(t *testing.T) {
	r := &SnapshotConfigRegistry{}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshot-configs", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.HandleCreateSnapshotConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST with empty commands: got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSnapshotConfigs_CreateInvalidJSON(t *testing.T) {
	r := &SnapshotConfigRegistry{}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshot-configs", strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.HandleCreateSnapshotConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST with invalid JSON: got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSnapshotConfigs_GetEmptyChunkKey(t *testing.T) {
	r := &SnapshotConfigRegistry{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot-configs/", nil)
	w := httptest.NewRecorder()
	r.HandleGetSnapshotConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("GET with empty chunk_key: got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSnapshotConfigs_CreateMethodNotAllowed(t *testing.T) {
	r := &SnapshotConfigRegistry{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot-configs", nil)
	w := httptest.NewRecorder()
	r.HandleCreateSnapshotConfig(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on create endpoint: got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleSnapshotConfigs_ListMethodNotAllowed(t *testing.T) {
	r := &SnapshotConfigRegistry{}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/snapshot-configs", nil)
	w := httptest.NewRecorder()
	r.HandleListSnapshotConfigs(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE on list endpoint: got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
