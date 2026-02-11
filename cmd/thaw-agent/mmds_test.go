package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	// Initialize the global logger that waitForMMDS and other functions depend on.
	log = logrus.New()
	log.SetLevel(logrus.DebugLevel)
	os.Exit(m.Run())
}

func TestWaitForMMDS_WrappedJSON(t *testing.T) {
	data := MMDSData{}
	data.Latest.Meta.RunnerID = "runner-abc"
	data.Latest.Meta.HostID = "host-123"
	data.Latest.Network.IP = "172.16.0.2/24"
	data.Latest.Job.Repo = "org/repo"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}))
	defer srv.Close()

	old := *mmdsEndpoint
	*mmdsEndpoint = srv.URL
	defer func() { *mmdsEndpoint = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := waitForMMDS(ctx)
	if err != nil {
		t.Fatalf("waitForMMDS() error = %v", err)
	}
	if got.Latest.Meta.RunnerID != "runner-abc" {
		t.Errorf("RunnerID = %q, want %q", got.Latest.Meta.RunnerID, "runner-abc")
	}
	if got.Latest.Meta.HostID != "host-123" {
		t.Errorf("HostID = %q, want %q", got.Latest.Meta.HostID, "host-123")
	}
	if got.Latest.Job.Repo != "org/repo" {
		t.Errorf("Repo = %q, want %q", got.Latest.Job.Repo, "org/repo")
	}
}

func TestWaitForMMDS_UnwrappedJSON(t *testing.T) {
	// MMDS sometimes returns data without the "latest" wrapper
	inner := struct {
		Meta struct {
			RunnerID    string `json:"runner_id"`
			HostID      string `json:"host_id"`
			Environment string `json:"environment"`
		} `json:"meta"`
		Network struct {
			IP string `json:"ip"`
		} `json:"network"`
		Job struct {
			Repo string `json:"repo"`
		} `json:"job"`
	}{}
	inner.Meta.RunnerID = "runner-unwrapped"
	inner.Meta.HostID = "host-456"
	inner.Network.IP = "10.0.0.5/24"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(inner)
	}))
	defer srv.Close()

	old := *mmdsEndpoint
	*mmdsEndpoint = srv.URL
	defer func() { *mmdsEndpoint = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := waitForMMDS(ctx)
	if err != nil {
		t.Fatalf("waitForMMDS() error = %v", err)
	}
	if got.Latest.Meta.RunnerID != "runner-unwrapped" {
		t.Errorf("RunnerID = %q, want %q", got.Latest.Meta.RunnerID, "runner-unwrapped")
	}
}

func TestWaitForMMDS_WaitsForRunnerID(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			// First calls: return empty runner_id
			json.NewEncoder(w).Encode(MMDSData{})
		} else {
			// Third call: return populated data
			data := MMDSData{}
			data.Latest.Meta.RunnerID = "runner-delayed"
			json.NewEncoder(w).Encode(data)
		}
	}))
	defer srv.Close()

	old := *mmdsEndpoint
	*mmdsEndpoint = srv.URL
	defer func() { *mmdsEndpoint = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := waitForMMDS(ctx)
	if err != nil {
		t.Fatalf("waitForMMDS() error = %v", err)
	}
	if got.Latest.Meta.RunnerID != "runner-delayed" {
		t.Errorf("RunnerID = %q, want %q", got.Latest.Meta.RunnerID, "runner-delayed")
	}
	if callCount.Load() < 3 {
		t.Errorf("expected at least 3 calls, got %d", callCount.Load())
	}
}

func TestWaitForMMDS_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Always return empty runner_id
		json.NewEncoder(w).Encode(MMDSData{})
	}))
	defer srv.Close()

	old := *mmdsEndpoint
	*mmdsEndpoint = srv.URL
	defer func() { *mmdsEndpoint = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := waitForMMDS(ctx)
	if err == nil {
		t.Error("waitForMMDS() expected error on context cancellation, got nil")
	}
}

func TestFetchMMDSData_Success(t *testing.T) {
	data := MMDSData{}
	data.Latest.Meta.RunnerID = "runner-fetch"
	data.Latest.Meta.HostID = "host-fetch"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}))
	defer srv.Close()

	old := *mmdsEndpoint
	*mmdsEndpoint = srv.URL
	defer func() { *mmdsEndpoint = old }()

	got, err := fetchMMDSData()
	if err != nil {
		t.Fatalf("fetchMMDSData() error = %v", err)
	}
	if got.Latest.Meta.RunnerID != "runner-fetch" {
		t.Errorf("RunnerID = %q, want %q", got.Latest.Meta.RunnerID, "runner-fetch")
	}
}

func TestFetchMMDSData_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := *mmdsEndpoint
	*mmdsEndpoint = srv.URL
	defer func() { *mmdsEndpoint = old }()

	_, err := fetchMMDSData()
	if err == nil {
		t.Error("fetchMMDSData() expected error on server error, got nil")
	}
}
