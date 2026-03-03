package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

func TestStaticTokenVerifier_ValidToken(t *testing.T) {
	verifier := StaticTokenVerifier("secret-token-123")
	info, err := verifier(context.Background(), "secret-token-123", &http.Request{})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil TokenInfo")
	}
	if info.Expiration.IsZero() {
		t.Fatal("expected non-zero expiration")
	}
}

func TestStaticTokenVerifier_InvalidToken(t *testing.T) {
	verifier := StaticTokenVerifier("secret-token-123")
	_, err := verifier(context.Background(), "wrong-token", &http.Request{})
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	// Should wrap ErrInvalidToken
	if !isInvalidTokenError(err) {
		t.Fatalf("expected ErrInvalidToken, got: %v", err)
	}
}

func TestStaticTokenVerifier_EmptyCandidate(t *testing.T) {
	verifier := StaticTokenVerifier("secret-token-123")
	_, err := verifier(context.Background(), "", &http.Request{})
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func isInvalidTokenError(err error) bool {
	for e := err; e != nil; e = unwrap(e) {
		if e == auth.ErrInvalidToken {
			return true
		}
	}
	return false
}

func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}
