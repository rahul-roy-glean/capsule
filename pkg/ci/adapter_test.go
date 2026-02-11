package ci

import (
	"context"
	"net/http"
	"testing"
)

// MockAdapter implements Adapter with settable function fields for testing.
type MockAdapter struct {
	NameFunc           func() string
	GetRunnerTokenFunc func(ctx context.Context, opts RunnerTokenOpts) (string, error)
	RunnerURLFunc      func() string
	OnDrainFunc        func(ctx context.Context, runners []RunnerInfo) error
	OnReleaseFunc      func(ctx context.Context, runner RunnerInfo) error
	WebhookHandlerFunc func() http.Handler
	CloseFunc          func() error
}

var _ Adapter = (*MockAdapter)(nil)

func (m *MockAdapter) Name() string {
	if m.NameFunc != nil {
		return m.NameFunc()
	}
	return "mock"
}

func (m *MockAdapter) GetRunnerToken(ctx context.Context, opts RunnerTokenOpts) (string, error) {
	if m.GetRunnerTokenFunc != nil {
		return m.GetRunnerTokenFunc(ctx, opts)
	}
	return "", nil
}

func (m *MockAdapter) RunnerURL() string {
	if m.RunnerURLFunc != nil {
		return m.RunnerURLFunc()
	}
	return ""
}

func (m *MockAdapter) OnDrain(ctx context.Context, runners []RunnerInfo) error {
	if m.OnDrainFunc != nil {
		return m.OnDrainFunc(ctx, runners)
	}
	return nil
}

func (m *MockAdapter) OnRelease(ctx context.Context, runner RunnerInfo) error {
	if m.OnReleaseFunc != nil {
		return m.OnReleaseFunc(ctx, runner)
	}
	return nil
}

func (m *MockAdapter) WebhookHandler() http.Handler {
	if m.WebhookHandlerFunc != nil {
		return m.WebhookHandlerFunc()
	}
	return nil
}

func (m *MockAdapter) Close() error {
	if m.CloseFunc != nil {
		return m.CloseFunc()
	}
	return nil
}

func TestNoopAdapter_Name(t *testing.T) {
	a := NewNoopAdapter()
	if got := a.Name(); got != "none" {
		t.Errorf("Name() = %q, want %q", got, "none")
	}
}

func TestNoopAdapter_GetRunnerToken(t *testing.T) {
	a := NewNoopAdapter()
	token, err := a.GetRunnerToken(context.Background(), RunnerTokenOpts{})
	if err != nil {
		t.Fatalf("GetRunnerToken() error = %v", err)
	}
	if token != "" {
		t.Errorf("GetRunnerToken() = %q, want empty string", token)
	}
}

func TestNoopAdapter_RunnerURL(t *testing.T) {
	a := NewNoopAdapter()
	if got := a.RunnerURL(); got != "" {
		t.Errorf("RunnerURL() = %q, want empty string", got)
	}
}

func TestNoopAdapter_OnDrain(t *testing.T) {
	a := NewNoopAdapter()
	runners := []RunnerInfo{{ID: "r1", Name: "runner-1"}}
	if err := a.OnDrain(context.Background(), runners); err != nil {
		t.Errorf("OnDrain() error = %v", err)
	}
}

func TestNoopAdapter_OnRelease(t *testing.T) {
	a := NewNoopAdapter()
	if err := a.OnRelease(context.Background(), RunnerInfo{ID: "r1"}); err != nil {
		t.Errorf("OnRelease() error = %v", err)
	}
}

func TestNoopAdapter_WebhookHandler(t *testing.T) {
	a := NewNoopAdapter()
	if h := a.WebhookHandler(); h != nil {
		t.Errorf("WebhookHandler() = %v, want nil", h)
	}
}

func TestNoopAdapter_Close(t *testing.T) {
	a := NewNoopAdapter()
	if err := a.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func TestNoopAdapter_ImplementsInterface(t *testing.T) {
	var _ Adapter = (*NoopAdapter)(nil)
}

func TestMockAdapter_Defaults(t *testing.T) {
	m := &MockAdapter{}
	if got := m.Name(); got != "mock" {
		t.Errorf("Name() = %q, want %q", got, "mock")
	}
	token, err := m.GetRunnerToken(context.Background(), RunnerTokenOpts{})
	if err != nil || token != "" {
		t.Errorf("GetRunnerToken() = (%q, %v), want (\"\", nil)", token, err)
	}
	if got := m.RunnerURL(); got != "" {
		t.Errorf("RunnerURL() = %q, want \"\"", got)
	}
	if err := m.OnDrain(context.Background(), nil); err != nil {
		t.Errorf("OnDrain() = %v", err)
	}
	if err := m.OnRelease(context.Background(), RunnerInfo{}); err != nil {
		t.Errorf("OnRelease() = %v", err)
	}
	if h := m.WebhookHandler(); h != nil {
		t.Errorf("WebhookHandler() = %v, want nil", h)
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
}

func TestMockAdapter_CustomFuncs(t *testing.T) {
	called := false
	m := &MockAdapter{
		NameFunc: func() string { return "custom" },
		GetRunnerTokenFunc: func(ctx context.Context, opts RunnerTokenOpts) (string, error) {
			called = true
			return "tok-123", nil
		},
	}
	if got := m.Name(); got != "custom" {
		t.Errorf("Name() = %q, want %q", got, "custom")
	}
	token, _ := m.GetRunnerToken(context.Background(), RunnerTokenOpts{})
	if !called || token != "tok-123" {
		t.Errorf("GetRunnerToken not called correctly")
	}
}
