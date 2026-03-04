package providers

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/authproxy"
)

func init() {
	authproxy.RegisterProvider("delegated", newDelegatedProvider)
}

// delegatedProvider accepts externally pushed tokens and injects them into
// matching requests. Implements both CredentialProvider and TokenReceiver.
type delegatedProvider struct {
	hosts  []string
	header string // HTTP header to set (e.g., "Authorization")
	prefix string // prefix before token (e.g., "token ", "Bearer ")

	mu        sync.RWMutex
	token     string
	expiresAt time.Time

	logger *logrus.Entry
}

func newDelegatedProvider(cfg authproxy.ProviderConfig) (authproxy.CredentialProvider, error) {
	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("delegated: at least one host is required")
	}

	header := cfg.Config["header"]
	if header == "" {
		header = "Authorization"
	}

	return &delegatedProvider{
		hosts:  cfg.Hosts,
		header: header,
		prefix: cfg.Config["prefix"],
		logger: logrus.WithField("provider", "delegated"),
	}, nil
}

func (p *delegatedProvider) Name() string { return "delegated" }

func (p *delegatedProvider) Matches(host string) bool {
	return slices.Contains(p.hosts, host)
}

func (p *delegatedProvider) InjectCredentials(req *http.Request) error {
	p.mu.RLock()
	tok := p.token
	exp := p.expiresAt
	p.mu.RUnlock()

	if tok == "" {
		return fmt.Errorf("delegated: no token available")
	}

	if !exp.IsZero() && time.Now().After(exp) {
		return fmt.Errorf("delegated: token expired at %s", exp.Format(time.RFC3339))
	}

	req.Header.Set(p.header, p.prefix+tok)
	return nil
}

func (p *delegatedProvider) Start(_ context.Context) error {
	return nil
}

func (p *delegatedProvider) Stop() {}

// UpdateToken implements authproxy.TokenReceiver.
func (p *delegatedProvider) UpdateToken(token string, expiresAt time.Time) error {
	if token == "" {
		return fmt.Errorf("delegated: token must not be empty")
	}

	p.mu.Lock()
	p.token = token
	p.expiresAt = expiresAt
	p.mu.Unlock()

	p.logger.WithField("expires_at", expiresAt.Format(time.RFC3339)).Debug("Token updated via push")
	return nil
}
