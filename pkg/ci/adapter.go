package ci

import (
	"context"
	"net/http"
)

// RunnerTokenOpts contains options for getting a runner registration token.
type RunnerTokenOpts struct {
	Repo   string
	Org    string
	Labels []string
}

// RunnerInfo describes a runner for CI adapter callbacks.
type RunnerInfo struct {
	ID     string
	Name   string
	Repo   string
	Labels []string
}

// Adapter is the interface for CI system integrations.
// Implementations handle runner registration, token generation, and webhooks.
type Adapter interface {
	// Name returns the CI system name (e.g., "github-actions", "none").
	Name() string

	// GetRunnerToken generates a runner registration token for the given options.
	// Returns empty string if no token is needed (e.g., no-op adapter).
	GetRunnerToken(ctx context.Context, opts RunnerTokenOpts) (string, error)

	// RunnerURL returns the URL to use for runner registration.
	RunnerURL() string

	// OnDrain is called when the host enters drain mode.
	// Implementations should prevent new jobs from being scheduled (e.g., remove labels).
	OnDrain(ctx context.Context, runners []RunnerInfo) error

	// OnRelease is called when a runner is released.
	OnRelease(ctx context.Context, runner RunnerInfo) error

	// WebhookHandler returns an HTTP handler for CI webhooks, or nil if not needed.
	WebhookHandler() http.Handler

	// Close releases resources held by the adapter.
	Close() error
}
