package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddleware_AllowsHealthWithoutToken(t *testing.T) {
	handler := authMiddleware("api-token", "host-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for /health, got %d", resp.Code)
	}
}

func TestAuthMiddleware_RequiresAPITokenForAPIPaths(t *testing.T) {
	handler := authMiddleware("api-token", "host-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runners", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without API token, got %d", resp.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode unauthorized response: %v", err)
	}
	if body["error"] != "unauthorized" {
		t.Fatalf("expected unauthorized error body, got %#v", body)
	}
}

func TestAuthMiddleware_AcceptsBearerForAPIPaths(t *testing.T) {
	handler := authMiddleware("api-token", "host-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runners", nil)
	req.Header.Set("Authorization", "Bearer api-token")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid bearer token, got %d", resp.Code)
	}
}

func TestAuthMiddleware_RequiresHostTokenForHeartbeatWhenConfigured(t *testing.T) {
	handler := authMiddleware("api-token", "host-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hosts/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer api-token")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when heartbeat uses API token, got %d", resp.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/hosts/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer host-token")
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 when heartbeat uses host token, got %d", resp.Code)
	}
}

func TestAuthMiddleware_LeavesHeartbeatOpenWhenHostTokenUnset(t *testing.T) {
	handler := authMiddleware("api-token", "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hosts/heartbeat", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 when host token is unset, got %d", resp.Code)
	}
}
