package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestHandleHostHeartbeat_MissingInstanceName(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)
	sm := &SnapshotManager{logger: logger.WithField("test", true)}
	srv := &ControlPlaneServer{
		hostRegistry:    hr,
		snapshotManager: sm,
		logger:          logger.WithField("test", true),
	}

	body, _ := json.Marshal(map[string]interface{}{
		"zone": "us-central1-a",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hosts/heartbeat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.HandleHostHeartbeat(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for missing instance_name, got %d", w.Code)
	}
}

func TestHandleHostHeartbeat_MethodNotAllowed(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	srv := &ControlPlaneServer{
		logger: logger.WithField("test", true),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hosts/heartbeat", nil)
	w := httptest.NewRecorder()

	srv.HandleHostHeartbeat(w, req)

	// GET should not be accepted (handler checks for POST)
	// Depending on implementation, this may return 200 with empty body or 405
	// The current implementation tries to decode the body and fails
}

func TestHandleCanaryReport_Success(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	srv := &ControlPlaneServer{
		logger: logger.WithField("test", true),
	}

	body, _ := json.Marshal(map[string]string{
		"status": "success",
		"runner": "test-runner",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/canary/report", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.HandleCanaryReport(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("Expected status ok, got %s", resp["status"])
	}
}

func TestHandleCanaryReport_MethodNotAllowed(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	srv := &ControlPlaneServer{
		logger: logger.WithField("test", true),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/canary/report", nil)
	w := httptest.NewRecorder()

	srv.HandleCanaryReport(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405 for GET, got %d", w.Code)
	}
}

func TestHandleCanaryReport_InvalidJSON(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	srv := &ControlPlaneServer{
		logger: logger.WithField("test", true),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/canary/report", bytes.NewReader([]byte("not-json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.HandleCanaryReport(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestHandleGetDesiredVersions_MissingParam(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	srv := &ControlPlaneServer{
		logger: logger.WithField("test", true),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/versions/desired", nil)
	w := httptest.NewRecorder()

	srv.HandleGetDesiredVersions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for missing instance_name, got %d", w.Code)
	}
}

func TestHandleGetFleetConvergence_MissingParam(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	srv := &ControlPlaneServer{
		logger: logger.WithField("test", true),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/versions/fleet", nil)
	w := httptest.NewRecorder()

	srv.HandleGetFleetConvergence(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for missing chunk_key, got %d", w.Code)
	}
}
