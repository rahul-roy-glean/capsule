//go:build linux
// +build linux

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

func TestSetupBuilderNetworkForwardsHealthPort(t *testing.T) {
	if os.Getenv("SNAPSHOT_BUILDER_PRIV_TESTS") == "" {
		t.Skip("Set SNAPSHOT_BUILDER_PRIV_TESTS=1 to run privileged builder network tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("Privileged builder network tests require root")
	}

	logger := logrus.New()
	builderNet, err := setupBuilderNetwork("test-builder", logger)
	if err != nil {
		t.Fatalf("setupBuilderNetwork returned error: %v", err)
	}
	defer func() {
		if cleanupErr := builderNet.cleanup(); cleanupErr != nil {
			t.Fatalf("builderNet.cleanup returned error: %v", cleanupErr)
		}
	}()

	if builderNet.tapName() != "tap-slot-0" {
		t.Fatalf("tapName = %q, want tap-slot-0", builderNet.tapName())
	}
	if builderNet.guestIP() == "" || builderNet.pollIP() == "" || builderNet.firecrackerNetNSPath() == "" {
		t.Fatalf("builder network missing expected addressing: guest=%q poll=%q netns=%q", builderNet.guestIP(), builderNet.pollIP(), builderNet.firecrackerNetNSPath())
	}

	var ln net.Listener
	if err := builderNet.manager.RunInNamespace(builderNet.resourceID, func() error {
		var listenErr error
		ln, listenErr = net.Listen("tcp", fmt.Sprintf("%s:%d", builderNet.guestIP(), snapshot.ThawAgentHealthPort))
		return listenErr
	}); err != nil {
		t.Fatalf("RunInNamespace listen returned error: %v", err)
	}
	defer ln.Close()

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}),
	}
	defer server.Close()
	go func() { _ = server.Serve(ln) }()

	url := fmt.Sprintf("http://%s:%d", builderNet.pollIP(), snapshot.ThawAgentHealthPort)
	client := &http.Client{Timeout: 1 * time.Second}
	var lastErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("host-reachable poll path never became healthy via %s: %v", url, lastErr)
}
