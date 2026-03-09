package authproxy

import (
	"context"
	"net/http"
	"time"
)

// CredentialProvider generates/receives and injects credentials for matching hosts.
type CredentialProvider interface {
	Name() string
	Matches(host string) bool
	InjectCredentials(req *http.Request) error
	Start(ctx context.Context) error // background refresh goroutines
	Stop()
}

// TokenReceiver is optionally implemented by providers that accept pushed tokens
// from an external auth system (e.g., action pack OAuth tokens).
type TokenReceiver interface {
	UpdateToken(token string, expiresAt time.Time) error
}

// MetadataHandler is optionally implemented by providers that serve their own
// HTTP API (e.g., GCP metadata endpoint) instead of injecting into proxied requests.
type MetadataHandler interface {
	ServeMetadata(w http.ResponseWriter, r *http.Request)
}

