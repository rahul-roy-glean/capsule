package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"

	"github.com/rahul-roy-glean/capsule/pkg/authproxy"
)

func init() {
	authproxy.RegisterProvider("token-exchange", newTokenExchangeProvider)
}

// tokenExchangeProvider mints a GCP OIDC identity token by impersonating a
// service account, exchanges it at an OAuth token endpoint (e.g., RFC 7523
// jwt-bearer grant), and injects the resulting JWT into matching requests.
type tokenExchangeProvider struct {
	hosts            []string
	serviceAccount   string // SA to impersonate for GCP ID token
	audience         string // audience for the GCP ID token
	exchangeEndpoint string // OAuth token endpoint URL
	grantType        string // e.g., "urn:ietf:params:oauth:grant-type:jwt-bearer"
	callerID         string // optional X-MCP-Caller-Id header value
	header           string // injection header (default: "Authorization")
	prefix           string // injection prefix (default: "Bearer ")

	mu        sync.RWMutex
	token     string
	expiresAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	logger *logrus.Entry
}

func newTokenExchangeProvider(cfg authproxy.ProviderConfig) (authproxy.CredentialProvider, error) {
	sa := cfg.Config["service_account"]
	if sa == "" {
		return nil, fmt.Errorf("token-exchange: service_account is required")
	}

	endpoint := cfg.Config["exchange_endpoint"]
	if endpoint == "" {
		return nil, fmt.Errorf("token-exchange: exchange_endpoint is required")
	}

	audience := cfg.Config["audience"]
	if audience == "" {
		return nil, fmt.Errorf("token-exchange: audience is required")
	}

	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("token-exchange: at least one host is required")
	}

	grantType := cfg.Config["grant_type"]
	if grantType == "" {
		grantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	}

	header := cfg.Config["header"]
	if header == "" {
		header = "Authorization"
	}

	prefix := cfg.Config["prefix"]
	if prefix == "" {
		prefix = "Bearer "
	}

	return &tokenExchangeProvider{
		hosts:            cfg.Hosts,
		serviceAccount:   sa,
		audience:         audience,
		exchangeEndpoint: endpoint,
		grantType:        grantType,
		callerID:         cfg.Config["caller_id"],
		header:           header,
		prefix:           prefix,
		logger:           logrus.WithField("provider", "token-exchange"),
	}, nil
}

func (p *tokenExchangeProvider) Name() string { return "token-exchange" }

func (p *tokenExchangeProvider) Matches(host string) bool {
	return slices.Contains(p.hosts, host)
}

func (p *tokenExchangeProvider) InjectCredentials(req *http.Request) error {
	p.mu.RLock()
	tok := p.token
	exp := p.expiresAt
	p.mu.RUnlock()

	if tok == "" || time.Now().After(exp) {
		if err := p.refreshToken(); err != nil {
			return fmt.Errorf("token-exchange: refresh failed: %w", err)
		}
		p.mu.RLock()
		tok = p.token
		p.mu.RUnlock()
	}

	if tok == "" {
		return fmt.Errorf("token-exchange: token not available")
	}

	req.Header.Set(p.header, p.prefix+tok)
	return nil
}

func (p *tokenExchangeProvider) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)

	if err := p.refreshToken(); err != nil {
		p.logger.WithError(err).Warn("Initial token exchange failed (will retry)")
	}

	go p.refreshLoop()
	return nil
}

func (p *tokenExchangeProvider) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

// refreshToken mints a GCP OIDC identity token and exchanges it for a JWT.
func (p *tokenExchangeProvider) refreshToken() error {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	// Step 1: Mint GCP OIDC identity token by impersonating the SA.
	idToken, err := p.mintIDToken(ctx)
	if err != nil {
		return fmt.Errorf("minting ID token: %w", err)
	}

	// Step 2: Exchange the ID token at the OAuth endpoint.
	tok, expiresIn, err := p.exchangeToken(ctx, idToken)
	if err != nil {
		return fmt.Errorf("exchanging token: %w", err)
	}

	expiry := time.Now().Add(time.Duration(expiresIn) * time.Second)

	p.mu.Lock()
	p.token = tok
	p.expiresAt = expiry
	p.mu.Unlock()

	p.logger.WithFields(logrus.Fields{
		"expires_at": expiry.Format(time.RFC3339),
		"endpoint":   p.exchangeEndpoint,
	}).Debug("Token exchange completed")
	return nil
}

func (p *tokenExchangeProvider) mintIDToken(ctx context.Context) (string, error) {
	svc, err := iamcredentials.NewService(ctx, option.WithScopes("https://www.googleapis.com/auth/cloud-platform"))
	if err != nil {
		return "", fmt.Errorf("creating IAM credentials service: %w", err)
	}

	name := fmt.Sprintf("projects/-/serviceAccounts/%s", p.serviceAccount)
	resp, err := svc.Projects.ServiceAccounts.GenerateIdToken(name,
		&iamcredentials.GenerateIdTokenRequest{
			Audience:     p.audience,
			IncludeEmail: true,
		}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("generating ID token for %s: %w", p.serviceAccount, err)
	}

	return resp.Token, nil
}

// exchangeToken POSTs the ID token to the exchange endpoint and returns
// the access token and its lifetime in seconds.
func (p *tokenExchangeProvider) exchangeToken(ctx context.Context, idToken string) (string, int, error) {
	form := url.Values{
		"grant_type": {p.grantType},
		"assertion":  {idToken},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.exchangeEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("creating exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.callerID != "" {
		req.Header.Set("X-MCP-Caller-Id", p.callerID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("reading exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("exchange returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenExchangeResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, fmt.Errorf("parsing exchange response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", 0, fmt.Errorf("exchange response missing access_token")
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600 // default 1 hour
	}

	return tokenResp.AccessToken, expiresIn, nil
}

type tokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

func (p *tokenExchangeProvider) refreshLoop() {
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
				p.logger.WithError(err).Warn("Token exchange refresh failed")
			}
		case <-p.ctx.Done():
			return
		}
	}
}
