package main

import (
	"strings"
	"testing"
)

func TestStripIngressDocuments(t *testing.T) {
	input := "---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: test\n---\napiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: my-ingress\nspec:\n  rules:\n  - host: example.com\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: my-deploy"

	result := stripIngressDocuments(input)

	if strings.Contains(result, "kind: Ingress") {
		t.Error("stripIngressDocuments did not remove Ingress document")
	}
	if !strings.Contains(result, "kind: Namespace") {
		t.Error("stripIngressDocuments removed Namespace document")
	}
	if !strings.Contains(result, "kind: Deployment") {
		t.Error("stripIngressDocuments removed Deployment document")
	}
}

func TestStripIngressDocuments_NoIngress(t *testing.T) {
	input := "---\napiVersion: v1\nkind: Service\nmetadata:\n  name: test"

	result := stripIngressDocuments(input)
	if !strings.Contains(result, "kind: Service") {
		t.Error("stripIngressDocuments removed Service when no Ingress present")
	}
}

func TestStripIngressDocuments_OnlyIngress(t *testing.T) {
	input := "---\napiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: my-ingress"

	result := stripIngressDocuments(input)
	trimmed := strings.TrimSpace(result)
	if trimmed != "---" {
		t.Errorf("expected only separator, got %q", trimmed)
	}
}
