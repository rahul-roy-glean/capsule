package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRepos_MethodRouting(t *testing.T) {
	// Without a real DB, we can only test routing and error cases.
	// The handlers will fail on DB access but we can verify they accept correct methods.

	// Test that POST to /api/v1/repos with missing body returns 400
	rr := &RepoRegistry{} // nil DB will cause panic on DB access, but validation should catch first

	// Test empty URL validation
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rr.HandleCreateRepo(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST with empty URL: got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleRepos_InvalidJSON(t *testing.T) {
	rr := &RepoRegistry{}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos", strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rr.HandleCreateRepo(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST with invalid JSON: got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleGetRepo_EmptySlug(t *testing.T) {
	rr := &RepoRegistry{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/", nil)
	w := httptest.NewRecorder()
	rr.HandleGetRepo(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("GET with empty slug: got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleUpdateRepo_MethodNotAllowed(t *testing.T) {
	rr := &RepoRegistry{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/test-repo", nil)
	w := httptest.NewRecorder()
	rr.HandleUpdateRepo(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on update endpoint: got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleListRepos_MethodNotAllowed(t *testing.T) {
	rr := &RepoRegistry{}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos", nil)
	w := httptest.NewRecorder()
	rr.HandleListRepos(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE on list endpoint: got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
