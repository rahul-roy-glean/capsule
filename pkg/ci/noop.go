package ci

import (
	"context"
	"net/http"
)

// NoopAdapter is a no-op CI adapter for when no CI system is configured.
type NoopAdapter struct{}

var _ Adapter = (*NoopAdapter)(nil)

func NewNoopAdapter() *NoopAdapter {
	return &NoopAdapter{}
}

func (n *NoopAdapter) Name() string { return "none" }

func (n *NoopAdapter) GetRunnerToken(ctx context.Context, opts RunnerTokenOpts) (string, error) {
	return "", nil
}

func (n *NoopAdapter) RunnerURL() string { return "" }

func (n *NoopAdapter) OnDrain(ctx context.Context, runners []RunnerInfo) error {
	return nil
}

func (n *NoopAdapter) OnRelease(ctx context.Context, runner RunnerInfo) error {
	return nil
}

func (n *NoopAdapter) WebhookHandler() http.Handler { return nil }

func (n *NoopAdapter) Close() error { return nil }
