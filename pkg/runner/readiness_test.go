package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaitForThawAgentExecURL_Succeeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exec" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/exec")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"stdout","data":"ready\n"}`))
	}))
	defer server.Close()

	if err := waitForThawAgentExecURL(context.Background(), server.URL+"/exec", 2*time.Second); err != nil {
		t.Fatalf("waitForThawAgentExecURL() error = %v", err)
	}
}

func TestWaitForThawAgentExecURL_TimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("not ready"))
	}))
	defer server.Close()

	err := waitForThawAgentExecURL(context.Background(), server.URL+"/exec", 600*time.Millisecond)
	if err == nil {
		t.Fatal("waitForThawAgentExecURL() error = nil, want timeout")
	}
	if !strings.Contains(err.Error(), "not ready after") {
		t.Fatalf("waitForThawAgentExecURL() error = %v, want timeout context", err)
	}
}

func TestWaitForThawAgentExecURL_RespectsContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForThawAgentExecURL(ctx, server.URL+"/exec", 2*time.Second)
	if err != context.Canceled {
		t.Fatalf("waitForThawAgentExecURL() error = %v, want %v", err, context.Canceled)
	}
}
