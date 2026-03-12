package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestHealthHandler_Returns200(t *testing.T) {
	handler := healthHandler(nil) // healthHandler doesn't use mgr
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("healthHandler status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "OK" {
		t.Errorf("healthHandler body = %q, want %q", w.Body.String(), "OK")
	}
}

func TestGCHandler_POST(t *testing.T) {
	logger := logrus.New()
	handler := gcHandler(nil, logger) // gcHandler doesn't use mgr for POST
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gc", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("gcHandler POST status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("gcHandler status = %q, want %q", resp["status"], "ok")
	}
}

func TestGCHandler_WrongMethod(t *testing.T) {
	logger := logrus.New()
	handler := gcHandler(nil, logger)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/gc", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("gcHandler GET status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
