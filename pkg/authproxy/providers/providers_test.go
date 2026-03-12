package providers

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rahul-roy-glean/capsule/pkg/authproxy"
)

// --- delegated provider tests ---

func TestDelegatedProvider_New(t *testing.T) {
	t.Run("requires hosts", func(t *testing.T) {
		_, err := newDelegatedProvider(authproxy.ProviderConfig{
			Type:   "delegated",
			Config: map[string]string{"header": "Authorization"},
		})
		if err == nil {
			t.Fatal("expected error for empty hosts")
		}
	})

	t.Run("defaults header to Authorization", func(t *testing.T) {
		p, err := newDelegatedProvider(authproxy.ProviderConfig{
			Type:   "delegated",
			Hosts:  []string{"github.com"},
			Config: map[string]string{},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		dp := p.(*delegatedProvider)
		if dp.header != "Authorization" {
			t.Errorf("header = %q, want %q", dp.header, "Authorization")
		}
	})

	t.Run("custom header and prefix", func(t *testing.T) {
		p, err := newDelegatedProvider(authproxy.ProviderConfig{
			Type:   "delegated",
			Hosts:  []string{"npm.pkg.github.com"},
			Config: map[string]string{"header": "X-Token", "prefix": "Bearer "},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		dp := p.(*delegatedProvider)
		if dp.header != "X-Token" {
			t.Errorf("header = %q, want %q", dp.header, "X-Token")
		}
		if dp.prefix != "Bearer " {
			t.Errorf("prefix = %q, want %q", dp.prefix, "Bearer ")
		}
	})
}

func TestDelegatedProvider_Name(t *testing.T) {
	p, _ := newDelegatedProvider(authproxy.ProviderConfig{
		Type:  "delegated",
		Hosts: []string{"example.com"},
	})
	if p.Name() != "delegated" {
		t.Errorf("Name() = %q, want %q", p.Name(), "delegated")
	}
}

func TestDelegatedProvider_Matches(t *testing.T) {
	p, _ := newDelegatedProvider(authproxy.ProviderConfig{
		Type:  "delegated",
		Hosts: []string{"github.com", "api.github.com"},
	})

	tests := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"api.github.com", true},
		{"gitlab.com", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := p.Matches(tt.host); got != tt.want {
				t.Errorf("Matches(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestDelegatedProvider_InjectCredentials(t *testing.T) {
	p, _ := newDelegatedProvider(authproxy.ProviderConfig{
		Type:   "delegated",
		Hosts:  []string{"github.com"},
		Config: map[string]string{"header": "Authorization", "prefix": "token "},
	})

	t.Run("no token available", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "https://github.com/api", nil)
		err := p.InjectCredentials(req)
		if err == nil {
			t.Fatal("expected error when no token available")
		}
	})

	t.Run("token injected", func(t *testing.T) {
		// Push a token.
		receiver := p.(authproxy.TokenReceiver)
		expires := time.Now().Add(1 * time.Hour)
		if err := receiver.UpdateToken("ghp_abc123", expires); err != nil {
			t.Fatalf("UpdateToken failed: %v", err)
		}

		req, _ := http.NewRequest("GET", "https://github.com/api", nil)
		if err := p.InjectCredentials(req); err != nil {
			t.Fatalf("InjectCredentials failed: %v", err)
		}

		got := req.Header.Get("Authorization")
		if got != "token ghp_abc123" {
			t.Errorf("Authorization = %q, want %q", got, "token ghp_abc123")
		}
	})

	t.Run("expired token rejected", func(t *testing.T) {
		receiver := p.(authproxy.TokenReceiver)
		expired := time.Now().Add(-1 * time.Hour)
		if err := receiver.UpdateToken("old_token", expired); err != nil {
			t.Fatalf("UpdateToken failed: %v", err)
		}

		req, _ := http.NewRequest("GET", "https://github.com/api", nil)
		err := p.InjectCredentials(req)
		if err == nil {
			t.Fatal("expected error for expired token")
		}
	})
}

func TestDelegatedProvider_UpdateToken(t *testing.T) {
	p, _ := newDelegatedProvider(authproxy.ProviderConfig{
		Type:  "delegated",
		Hosts: []string{"github.com"},
	})
	receiver := p.(authproxy.TokenReceiver)

	t.Run("empty token rejected", func(t *testing.T) {
		err := receiver.UpdateToken("", time.Now().Add(1*time.Hour))
		if err == nil {
			t.Fatal("expected error for empty token")
		}
	})

	t.Run("valid token accepted", func(t *testing.T) {
		expires := time.Now().Add(1 * time.Hour)
		err := receiver.UpdateToken("valid-token", expires)
		if err != nil {
			t.Fatalf("UpdateToken failed: %v", err)
		}

		dp := p.(*delegatedProvider)
		dp.mu.RLock()
		defer dp.mu.RUnlock()
		if dp.token != "valid-token" {
			t.Errorf("token = %q, want %q", dp.token, "valid-token")
		}
	})
}

func TestDelegatedProvider_StartStop(t *testing.T) {
	p, _ := newDelegatedProvider(authproxy.ProviderConfig{
		Type:  "delegated",
		Hosts: []string{"example.com"},
	})
	// Start and Stop should be no-ops for delegated (no goroutines).
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	p.Stop()
}

func TestDelegatedProvider_InjectWithCustomHeader(t *testing.T) {
	p, _ := newDelegatedProvider(authproxy.ProviderConfig{
		Type:   "delegated",
		Hosts:  []string{"npm.pkg.github.com"},
		Config: map[string]string{"header": "X-NPM-Token", "prefix": ""},
	})

	receiver := p.(authproxy.TokenReceiver)
	if err := receiver.UpdateToken("npm_secret", time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("UpdateToken failed: %v", err)
	}

	req, _ := http.NewRequest("GET", "https://npm.pkg.github.com/pkg", nil)
	if err := p.InjectCredentials(req); err != nil {
		t.Fatalf("InjectCredentials failed: %v", err)
	}

	got := req.Header.Get("X-NPM-Token")
	if got != "npm_secret" {
		t.Errorf("X-NPM-Token = %q, want %q", got, "npm_secret")
	}
	// Authorization should not be set.
	if auth := req.Header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want empty", auth)
	}
}

// --- bearer-token provider tests (config only, no Secret Manager) ---

func TestBearerTokenProvider_New(t *testing.T) {
	t.Run("requires secret_ref", func(t *testing.T) {
		_, err := newBearerTokenProvider(authproxy.ProviderConfig{
			Type:  "bearer-token",
			Hosts: []string{"api.example.com"},
		})
		if err == nil {
			t.Fatal("expected error for missing secret_ref")
		}
	})

	t.Run("requires hosts", func(t *testing.T) {
		_, err := newBearerTokenProvider(authproxy.ProviderConfig{
			Type:   "bearer-token",
			Config: map[string]string{"secret_ref": "sm://proj/secret"},
		})
		if err == nil {
			t.Fatal("expected error for empty hosts")
		}
	})

	t.Run("valid config", func(t *testing.T) {
		p, err := newBearerTokenProvider(authproxy.ProviderConfig{
			Type:   "bearer-token",
			Hosts:  []string{"api.example.com"},
			Config: map[string]string{"secret_ref": "sm://proj/secret"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Name() != "bearer-token" {
			t.Errorf("Name() = %q, want %q", p.Name(), "bearer-token")
		}
	})

	t.Run("custom refresh_ttl", func(t *testing.T) {
		p, err := newBearerTokenProvider(authproxy.ProviderConfig{
			Type:   "bearer-token",
			Hosts:  []string{"api.example.com"},
			Config: map[string]string{"secret_ref": "test", "refresh_ttl": "30m"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		bp := p.(*bearerTokenProvider)
		if bp.refreshTTL != 30*time.Minute {
			t.Errorf("refreshTTL = %v, want %v", bp.refreshTTL, 30*time.Minute)
		}
	})

	t.Run("invalid refresh_ttl", func(t *testing.T) {
		_, err := newBearerTokenProvider(authproxy.ProviderConfig{
			Type:   "bearer-token",
			Hosts:  []string{"api.example.com"},
			Config: map[string]string{"secret_ref": "test", "refresh_ttl": "not-a-duration"},
		})
		if err == nil {
			t.Fatal("expected error for invalid refresh_ttl")
		}
	})
}

func TestBearerTokenProvider_Matches(t *testing.T) {
	p, _ := newBearerTokenProvider(authproxy.ProviderConfig{
		Type:   "bearer-token",
		Hosts:  []string{"api.example.com", "cdn.example.com"},
		Config: map[string]string{"secret_ref": "test"},
	})

	if !p.Matches("api.example.com") {
		t.Error("expected Matches(api.example.com) = true")
	}
	if !p.Matches("cdn.example.com") {
		t.Error("expected Matches(cdn.example.com) = true")
	}
	if p.Matches("other.com") {
		t.Error("expected Matches(other.com) = false")
	}
}

func TestBearerTokenProvider_InjectWithoutToken(t *testing.T) {
	p, _ := newBearerTokenProvider(authproxy.ProviderConfig{
		Type:   "bearer-token",
		Hosts:  []string{"api.example.com"},
		Config: map[string]string{"secret_ref": "test"},
	})

	req, _ := http.NewRequest("GET", "https://api.example.com/v1", nil)
	err := p.InjectCredentials(req)
	if err == nil {
		t.Fatal("expected error when no token available")
	}
}

// --- github-app provider tests (config only, no GCP) ---

func TestGitHubAppProvider_New(t *testing.T) {
	t.Run("requires app_id", func(t *testing.T) {
		_, err := newGitHubAppProvider(authproxy.ProviderConfig{
			Type:   "github-app",
			Config: map[string]string{"secret_ref": "test"},
		})
		if err == nil {
			t.Fatal("expected error for missing app_id")
		}
	})

	t.Run("requires secret_ref", func(t *testing.T) {
		_, err := newGitHubAppProvider(authproxy.ProviderConfig{
			Type:   "github-app",
			Config: map[string]string{"app_id": "123"},
		})
		if err == nil {
			t.Fatal("expected error for missing secret_ref")
		}
	})

	t.Run("default hosts", func(t *testing.T) {
		p, err := newGitHubAppProvider(authproxy.ProviderConfig{
			Type:   "github-app",
			Config: map[string]string{"app_id": "123", "secret_ref": "sm://proj/key"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !p.Matches("github.com") {
			t.Error("expected Matches(github.com) = true")
		}
		if !p.Matches("api.github.com") {
			t.Error("expected Matches(api.github.com) = true")
		}
	})

	t.Run("custom hosts", func(t *testing.T) {
		p, err := newGitHubAppProvider(authproxy.ProviderConfig{
			Type:   "github-app",
			Hosts:  []string{"custom.github.enterprise.com"},
			Config: map[string]string{"app_id": "123", "secret_ref": "sm://proj/key"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !p.Matches("custom.github.enterprise.com") {
			t.Error("expected Matches(custom.github.enterprise.com) = true")
		}
		if p.Matches("github.com") {
			t.Error("expected Matches(github.com) = false for custom hosts")
		}
	})
}

// --- gcp-metadata provider tests (config only, no IAM) ---

func TestGCPMetadataProvider_New(t *testing.T) {
	t.Run("requires service_account", func(t *testing.T) {
		_, err := newGCPMetadataProvider(authproxy.ProviderConfig{
			Type: "gcp-metadata",
		})
		if err == nil {
			t.Fatal("expected error for missing service_account")
		}
	})

	t.Run("valid config with defaults", func(t *testing.T) {
		p, err := newGCPMetadataProvider(authproxy.ProviderConfig{
			Type: "gcp-metadata",
			Config: map[string]string{
				"service_account": "sa@project.iam.gserviceaccount.com",
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		gp := p.(*gcpMetadataProvider)
		if gp.serviceAccount != "sa@project.iam.gserviceaccount.com" {
			t.Errorf("serviceAccount = %q", gp.serviceAccount)
		}
		if len(gp.scopes) != 1 || gp.scopes[0] != "https://www.googleapis.com/auth/cloud-platform" {
			t.Errorf("scopes = %v, want default scope", gp.scopes)
		}
	})

	t.Run("custom scopes", func(t *testing.T) {
		p, err := newGCPMetadataProvider(authproxy.ProviderConfig{
			Type: "gcp-metadata",
			Config: map[string]string{
				"service_account": "sa@project.iam.gserviceaccount.com",
				"scopes":          "https://www.googleapis.com/auth/devstorage.read_only, https://www.googleapis.com/auth/logging.write",
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		gp := p.(*gcpMetadataProvider)
		if len(gp.scopes) != 2 {
			t.Errorf("scopes len = %d, want 2", len(gp.scopes))
		}
	})
}

func TestGCPMetadataProvider_MatchesFalse(t *testing.T) {
	p, _ := newGCPMetadataProvider(authproxy.ProviderConfig{
		Type:   "gcp-metadata",
		Config: map[string]string{"service_account": "sa@project.iam.gserviceaccount.com"},
	})
	// GCP metadata provider serves its own API — Matches returns false.
	if p.Matches("169.254.169.254") {
		t.Error("expected Matches to return false for metadata provider")
	}
	if p.Matches("github.com") {
		t.Error("expected Matches to return false for any host")
	}
}

func TestGCPMetadataProvider_Name(t *testing.T) {
	p, _ := newGCPMetadataProvider(authproxy.ProviderConfig{
		Type:   "gcp-metadata",
		Config: map[string]string{"service_account": "sa@project.iam.gserviceaccount.com"},
	})
	if p.Name() != "gcp-metadata" {
		t.Errorf("Name() = %q, want %q", p.Name(), "gcp-metadata")
	}
}

// --- init() registration tests ---

func TestProvidersRegistered(t *testing.T) {
	// The init() functions in each provider file should have registered them.
	types := []string{"gcp-metadata", "github-app", "bearer-token", "delegated"}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			if _, ok := authproxy.GetRegisteredProvider(typ); !ok {
				t.Errorf("provider %q not registered", typ)
			}
		})
	}
}
