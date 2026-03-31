package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rahul-roy-glean/capsule/pkg/authproxy"
)

func TestNewTokenExchangeProvider_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     authproxy.ProviderConfig
		wantErr string
	}{
		{
			name: "missing service_account",
			cfg: authproxy.ProviderConfig{
				Type:   "token-exchange",
				Hosts:  []string{"example.com"},
				Config: map[string]string{"exchange_endpoint": "https://x/token", "audience": "https://x"},
			},
			wantErr: "service_account is required",
		},
		{
			name: "missing exchange_endpoint",
			cfg: authproxy.ProviderConfig{
				Type:   "token-exchange",
				Hosts:  []string{"example.com"},
				Config: map[string]string{"service_account": "sa@proj.iam.gserviceaccount.com", "audience": "https://x"},
			},
			wantErr: "exchange_endpoint is required",
		},
		{
			name: "missing audience",
			cfg: authproxy.ProviderConfig{
				Type:   "token-exchange",
				Hosts:  []string{"example.com"},
				Config: map[string]string{"service_account": "sa@proj.iam.gserviceaccount.com", "exchange_endpoint": "https://x/token"},
			},
			wantErr: "audience is required",
		},
		{
			name: "missing hosts",
			cfg: authproxy.ProviderConfig{
				Type:   "token-exchange",
				Hosts:  []string{},
				Config: map[string]string{"service_account": "sa@proj.iam.gserviceaccount.com", "exchange_endpoint": "https://x/token", "audience": "https://x"},
			},
			wantErr: "at least one host is required",
		},
		{
			name: "valid config",
			cfg: authproxy.ProviderConfig{
				Type:  "token-exchange",
				Hosts: []string{"example.com"},
				Config: map[string]string{
					"service_account":   "sa@proj.iam.gserviceaccount.com",
					"exchange_endpoint": "https://x/token",
					"audience":          "https://x",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTokenExchangeProvider(tt.cfg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestTokenExchangeProvider_Matches(t *testing.T) {
	p := &tokenExchangeProvider{
		hosts: []string{"mcp.glean.com", "api.example.com"},
	}

	if !p.Matches("mcp.glean.com") {
		t.Error("expected match for mcp.glean.com")
	}
	if !p.Matches("api.example.com") {
		t.Error("expected match for api.example.com")
	}
	if p.Matches("other.com") {
		t.Error("unexpected match for other.com")
	}
}

func TestTokenExchangeProvider_InjectCredentials(t *testing.T) {
	p := &tokenExchangeProvider{}
	p.token = "test-jwt-token"
	p.expiresAt = time.Now().Add(1 * time.Hour)

	req := httptest.NewRequest(http.MethodGet, "https://mcp.glean.com/api", nil)
	if err := p.InjectCredentials(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := req.Header.Get("Authorization")
	if got != "Bearer test-jwt-token" {
		t.Fatalf("expected 'Bearer test-jwt-token', got %q", got)
	}
}

func TestTokenExchangeProvider_InjectCredentials_NoToken(t *testing.T) {
	p := &tokenExchangeProvider{
		exchangeEndpoint: "http://localhost:0/token", // unreachable
		serviceAccount:   "sa@proj.iam.gserviceaccount.com",
		audience:         "https://x",
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer p.cancel()

	req := httptest.NewRequest(http.MethodGet, "https://example.com/api", nil)
	err := p.InjectCredentials(req)
	if err == nil {
		t.Fatal("expected error when no token available")
	}
}

func TestTokenExchangeProvider_ExchangeToken(t *testing.T) {
	// Mock exchange endpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("expected form content type, got %s", ct)
		}

		if err := r.ParseForm(); err != nil {
			t.Fatalf("parsing form: %v", err)
		}
		if gt := r.FormValue("grant_type"); gt != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("expected jwt-bearer grant type, got %s", gt)
		}
		if assertion := r.FormValue("assertion"); assertion != "mock-id-token" {
			t.Errorf("expected mock-id-token assertion, got %s", assertion)
		}
		if callerID := r.Header.Get("X-MCP-Caller-Id"); callerID != "test-caller" {
			t.Errorf("expected test-caller caller ID, got %s", callerID)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenExchangeResponse{
			AccessToken: "exchanged-jwt",
			TokenType:   "Bearer",
			ExpiresIn:   3600,
		})
	}))
	defer server.Close()

	p := &tokenExchangeProvider{
		exchangeEndpoint: server.URL,
		callerID:         "test-caller",
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer p.cancel()

	tok, expiresIn, err := p.exchangeToken(context.Background(), "mock-id-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "exchanged-jwt" {
		t.Errorf("expected 'exchanged-jwt', got %q", tok)
	}
	if expiresIn != 3600 {
		t.Errorf("expected 3600, got %d", expiresIn)
	}
}

func TestTokenExchangeProvider_ExchangeToken_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "invalid_grant"}`))
	}))
	defer server.Close()

	p := &tokenExchangeProvider{
		exchangeEndpoint: server.URL,
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer p.cancel()

	_, _, err := p.exchangeToken(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !contains(err.Error(), "401") {
		t.Errorf("expected error mentioning 401, got %q", err.Error())
	}
}

func TestTokenExchangeProvider_ExchangeToken_DefaultExpiry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "tok",
			"token_type":   "Bearer",
		})
	}))
	defer server.Close()

	p := &tokenExchangeProvider{
		exchangeEndpoint: server.URL,
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer p.cancel()

	_, expiresIn, err := p.exchangeToken(context.Background(), "id-tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expiresIn != 3600 {
		t.Errorf("expected default 3600, got %d", expiresIn)
	}
}

func TestTokenExchangeProvider_Name(t *testing.T) {
	p := &tokenExchangeProvider{}
	if p.Name() != "token-exchange" {
		t.Errorf("expected 'token-exchange', got %q", p.Name())
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
