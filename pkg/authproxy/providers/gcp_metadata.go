package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"

	"github.com/rahul-roy-glean/capsule/pkg/authproxy"
)

func init() {
	authproxy.RegisterProvider("gcp-metadata", newGCPMetadataProvider)
}

// gcpMetadataProvider emulates the GCP metadata server's token endpoint
// by impersonating a configured service account via IAM generateAccessToken.
type gcpMetadataProvider struct {
	serviceAccount string
	scopes         []string
	projectID      string

	mu        sync.RWMutex
	token     string
	expiresAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	logger *logrus.Entry
}

func newGCPMetadataProvider(cfg authproxy.ProviderConfig) (authproxy.CredentialProvider, error) {
	sa := cfg.Config["service_account"]
	if sa == "" {
		return nil, fmt.Errorf("gcp-metadata: service_account is required")
	}

	scopes := []string{"https://www.googleapis.com/auth/cloud-platform"}
	if s, ok := cfg.Config["scopes"]; ok && s != "" {
		scopes = strings.Split(s, ",")
		for i := range scopes {
			scopes[i] = strings.TrimSpace(scopes[i])
		}
	}

	// Auto-extract project ID from service account email (user@PROJECT.iam.gserviceaccount.com)
	// when not explicitly configured. google-auth's default() calls get_project_id() on the
	// metadata server; returning an empty string with 200 causes a KeyError in google-auth
	// because Go's HTTP server omits Content-Type for zero-byte responses.
	projectID := cfg.Config["project_id"]
	if projectID == "" {
		if parts := strings.SplitN(sa, "@", 2); len(parts) == 2 {
			domain := parts[1]
			if strings.HasSuffix(domain, ".iam.gserviceaccount.com") {
				projectID = strings.TrimSuffix(domain, ".iam.gserviceaccount.com")
			}
		}
	}

	return &gcpMetadataProvider{
		serviceAccount: sa,
		scopes:         scopes,
		projectID:      projectID,
		logger:         logrus.WithField("provider", "gcp-metadata"),
	}, nil
}

func (p *gcpMetadataProvider) Name() string { return "gcp-metadata" }

// Matches returns false because the metadata provider serves its own HTTP API
// rather than injecting credentials into proxied requests.
func (p *gcpMetadataProvider) Matches(_ string) bool { return false }

// InjectCredentials is not used by the metadata provider.
func (p *gcpMetadataProvider) InjectCredentials(_ *http.Request) error { return nil }

func (p *gcpMetadataProvider) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)

	// Pre-fetch initial token.
	if err := p.refreshToken(); err != nil {
		p.logger.WithError(err).Warn("Initial token fetch failed (will retry)")
	}

	// Background refresh at 75% of token lifetime.
	go p.refreshLoop()
	return nil
}

func (p *gcpMetadataProvider) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

// ServeMetadata implements authproxy.MetadataHandler.
// Emulates GCP metadata endpoints used by client libraries.
func (p *gcpMetadataProvider) ServeMetadata(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// All GCP metadata responses include this header.
	w.Header().Set("Metadata-Flavor", "Google")

	switch {
	case path == "/" || path == "/computeMetadata/v1/" || path == "/computeMetadata/v1":
		// Ping endpoint — google-auth checks this to detect metadata server availability.
		// Must return 200 + Metadata-Flavor: Google header.
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok")
	case strings.HasSuffix(path, "/token"):
		p.serveToken(w)
	case strings.HasSuffix(path, "/email"):
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, p.serviceAccount)
	case strings.HasSuffix(path, "/scopes"):
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Join(p.scopes, "\n"))
	case path == "/computeMetadata/v1/project/project-id":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, p.projectID)
	case path == "/computeMetadata/v1/project/numeric-project-id":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "0")
	case p.isServiceAccountInfoPath(path):
		// google-auth's credentials.refresh() calls get_service_account_info()
		// which requests /instance/service-accounts/{account}/?recursive=true.
		// Return JSON with email, scopes, and aliases so refresh succeeds.
		p.serveServiceAccountInfo(w)
	default:
		http.NotFound(w, r)
	}
}

type metadataTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

func (p *gcpMetadataProvider) serveToken(w http.ResponseWriter) {
	p.mu.RLock()
	tok := p.token
	exp := p.expiresAt
	p.mu.RUnlock()

	if tok == "" || time.Now().After(exp) {
		// Try a synchronous refresh.
		if err := p.refreshToken(); err != nil {
			http.Error(w, "token unavailable", http.StatusServiceUnavailable)
			return
		}
		p.mu.RLock()
		tok = p.token
		exp = p.expiresAt
		p.mu.RUnlock()
	}

	remaining := max(int(time.Until(exp).Seconds()), 0)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metadataTokenResponse{
		AccessToken: tok,
		ExpiresIn:   remaining,
		TokenType:   "Bearer",
	})
}

func (p *gcpMetadataProvider) refreshToken() error {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	svc, err := iamcredentials.NewService(ctx, option.WithScopes(p.scopes...))
	if err != nil {
		return fmt.Errorf("creating IAM credentials service: %w", err)
	}

	name := fmt.Sprintf("projects/-/serviceAccounts/%s", p.serviceAccount)
	resp, err := svc.Projects.ServiceAccounts.GenerateAccessToken(name,
		&iamcredentials.GenerateAccessTokenRequest{
			Scope: p.scopes,
		}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("generating access token: %w", err)
	}

	expiry, err := time.Parse(time.RFC3339, resp.ExpireTime)
	if err != nil {
		expiry = time.Now().Add(3600 * time.Second)
	}

	p.mu.Lock()
	p.token = resp.AccessToken
	p.expiresAt = expiry
	p.mu.Unlock()

	p.logger.WithField("expires_at", expiry.Format(time.RFC3339)).Debug("Refreshed GCP access token")
	return nil
}

func (p *gcpMetadataProvider) refreshLoop() {
	for {
		p.mu.RLock()
		exp := p.expiresAt
		p.mu.RUnlock()

		// Refresh at 75% of remaining lifetime, minimum 30s.
		remaining := time.Until(exp)
		wait := max(remaining*3/4, 30*time.Second)

		select {
		case <-time.After(wait):
			if err := p.refreshToken(); err != nil {
				p.logger.WithError(err).Warn("Token refresh failed")
			}
		case <-p.ctx.Done():
			return
		}
	}
}

// isServiceAccountInfoPath returns true for paths like
// /computeMetadata/v1/instance/service-accounts/default/
// /computeMetadata/v1/instance/service-accounts/{email}/
// which google-auth's get_service_account_info() requests with ?recursive=true.
func (p *gcpMetadataProvider) isServiceAccountInfoPath(path string) bool {
	const prefix = "/computeMetadata/v1/instance/service-accounts/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	// Must end with "/" (e.g., "default/" or "sa@proj.iam.gserviceaccount.com/")
	// and not contain further path segments after the account name.
	return strings.HasSuffix(rest, "/") && !strings.Contains(rest[:len(rest)-1], "/")
}

// serveServiceAccountInfo returns JSON matching the GCE metadata recursive
// service account endpoint. google-auth's Credentials.refresh() calls
// _metadata.get_service_account_info() which expects this format.
func (p *gcpMetadataProvider) serveServiceAccountInfo(w http.ResponseWriter) {
	info := map[string]any{
		"aliases": []string{"default"},
		"email":   p.serviceAccount,
		"scopes":  p.scopes,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}
