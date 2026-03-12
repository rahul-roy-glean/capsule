package providers

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/authproxy"
)

func init() {
	authproxy.RegisterProvider("bearer-token", newBearerTokenProvider)
}

// bearerTokenProvider fetches a token from GCP Secret Manager and injects it
// as a Bearer Authorization header for matching hosts.
type bearerTokenProvider struct {
	hosts      []string
	secretRef  string // "sm://project/secret" or full resource name
	gcpProject string
	refreshTTL time.Duration // how often to re-read the secret (0 = once)

	mu    sync.RWMutex
	token string

	ctx    context.Context
	cancel context.CancelFunc
	logger *logrus.Entry
}

func newBearerTokenProvider(cfg authproxy.ProviderConfig) (authproxy.CredentialProvider, error) {
	secretRef := cfg.Config["secret_ref"]
	if secretRef == "" {
		return nil, fmt.Errorf("bearer-token: secret_ref is required")
	}

	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("bearer-token: at least one host is required")
	}

	refreshTTL := 1 * time.Hour
	if v, ok := cfg.Config["refresh_ttl"]; ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("bearer-token: invalid refresh_ttl %q: %w", v, err)
		}
		refreshTTL = d
	}

	return &bearerTokenProvider{
		hosts:      cfg.Hosts,
		secretRef:  secretRef,
		gcpProject: cfg.Config["gcp_project"],
		refreshTTL: refreshTTL,
		logger:     logrus.WithField("provider", "bearer-token"),
	}, nil
}

func (p *bearerTokenProvider) Name() string { return "bearer-token" }

func (p *bearerTokenProvider) Matches(host string) bool {
	return slices.Contains(p.hosts, host)
}

func (p *bearerTokenProvider) InjectCredentials(req *http.Request) error {
	p.mu.RLock()
	tok := p.token
	p.mu.RUnlock()

	if tok == "" {
		return fmt.Errorf("bearer-token: token not available")
	}

	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func (p *bearerTokenProvider) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)

	if err := p.fetchSecret(); err != nil {
		return fmt.Errorf("bearer-token: initial secret fetch: %w", err)
	}

	if p.refreshTTL > 0 {
		go p.refreshLoop()
	}
	return nil
}

func (p *bearerTokenProvider) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *bearerTokenProvider) fetchSecret() error {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("creating secret manager client: %w", err)
	}
	defer client.Close()

	secretPath := p.secretRef
	if rest, ok := strings.CutPrefix(secretPath, "sm://"); ok {
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 {
			secretPath = fmt.Sprintf("projects/%s/secrets/%s/versions/latest", parts[0], parts[1])
		}
	} else if !strings.HasPrefix(secretPath, "projects/") {
		secretPath = fmt.Sprintf("projects/%s/secrets/%s/versions/latest", p.gcpProject, secretPath)
	}

	result, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: secretPath,
	})
	if err != nil {
		return fmt.Errorf("accessing secret %s: %w", secretPath, err)
	}

	p.mu.Lock()
	p.token = string(result.Payload.Data)
	p.mu.Unlock()

	p.logger.Debug("Fetched bearer token from Secret Manager")
	return nil
}

func (p *bearerTokenProvider) refreshLoop() {
	for {
		select {
		case <-time.After(p.refreshTTL):
			if err := p.fetchSecret(); err != nil {
				p.logger.WithError(err).Warn("Bearer token refresh failed")
			}
		case <-p.ctx.Done():
			return
		}
	}
}
