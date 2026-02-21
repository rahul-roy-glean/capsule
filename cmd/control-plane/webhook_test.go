package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifySignature_Valid(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"test": "data"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	h := &GitHubWebhookHandler{webhookSecret: secret}
	if !h.verifySignature(body, sig) {
		t.Error("verifySignature returned false for valid signature")
	}
}

func TestVerifySignature_Invalid(t *testing.T) {
	h := &GitHubWebhookHandler{webhookSecret: "test-secret"}
	if h.verifySignature([]byte("data"), "sha256=invalid") {
		t.Error("verifySignature returned true for invalid signature")
	}
}

func TestVerifySignature_MissingPrefix(t *testing.T) {
	h := &GitHubWebhookHandler{webhookSecret: "test-secret"}
	if h.verifySignature([]byte("data"), "invalid-no-prefix") {
		t.Error("verifySignature returned true for signature without sha256= prefix")
	}
}

func TestShouldHandleJob(t *testing.T) {
	h := &GitHubWebhookHandler{}

	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"firecracker label", []string{"firecracker"}, true},
		{"self-hosted label", []string{"self-hosted"}, true},
		{"both labels", []string{"self-hosted", "firecracker"}, true},
		{"no matching labels", []string{"ubuntu-latest"}, false},
		{"empty labels", []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &WorkflowJobEvent{}
			event.WorkflowJob.Labels = tt.labels
			got := h.shouldHandleJob(event)
			if got != tt.want {
				t.Errorf("shouldHandleJob(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

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
