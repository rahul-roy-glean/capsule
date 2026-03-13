package providers

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/authproxy"
	"github.com/rahul-roy-glean/capsule/pkg/github"
)

func init() {
	authproxy.RegisterProvider("github-app", newGitHubAppProvider)
}

// githubAppProvider generates GitHub App installation tokens and injects
// them as Authorization headers for matching hosts.
type githubAppProvider struct {
	hosts          []string
	repos          []string // optional: only inject credentials for these repos (e.g. "owner/repo")
	appID          string
	secretRef      string // "sm://project/secret" or plain secret name
	installationID string // optional: specific installation ID
	gcpProject     string

	mu        sync.RWMutex
	token     string
	expiresAt time.Time
	client    *github.TokenClient

	ctx    context.Context
	cancel context.CancelFunc
	logger *logrus.Entry
}

func newGitHubAppProvider(cfg authproxy.ProviderConfig) (authproxy.CredentialProvider, error) {
	appID := cfg.Config["app_id"]
	if appID == "" {
		return nil, fmt.Errorf("github-app: app_id is required")
	}

	secretRef := cfg.Config["secret_ref"]
	if secretRef == "" {
		return nil, fmt.Errorf("github-app: secret_ref is required")
	}

	hosts := cfg.Hosts
	if len(hosts) == 0 {
		hosts = []string{"github.com", "api.github.com"}
	}

	// Parse repos list from comma-separated config value (e.g. "owner/repo,owner/other").
	var repos []string
	if r := cfg.Config["repos"]; r != "" {
		for _, repo := range strings.Split(r, ",") {
			repo = strings.TrimSpace(repo)
			if repo != "" {
				repos = append(repos, repo)
			}
		}
	}

	return &githubAppProvider{
		hosts:          hosts,
		repos:          repos,
		appID:          appID,
		secretRef:      secretRef,
		installationID: cfg.Config["installation_id"],
		gcpProject:     cfg.Config["gcp_project"],
		logger:         logrus.WithField("provider", "github-app"),
	}, nil
}

func (p *githubAppProvider) Name() string { return "github-app" }

func (p *githubAppProvider) Matches(host string) bool {
	return slices.Contains(p.hosts, host)
}

func (p *githubAppProvider) InjectCredentials(req *http.Request) error {
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}

	isAPI := strings.Contains(host, "api.github.com")

	if isAPI {
	} else {
		if len(p.repos) > 0 && !p.matchesRepo(req) {
			return nil
		}
	}

	token, err := p.getToken()
	if err != nil {
		return err
	}

	if isAPI {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.SetBasicAuth("x-access-token", token)
	}
	return nil
}

// matchesRepo checks if the request URL targets one of the configured repos.
// Matches patterns:
//   - github.com/{owner}/{repo}[/...]
//   - api.github.com/repos/{owner}/{repo}[/...]
func (p *githubAppProvider) matchesRepo(req *http.Request) bool {
	path := strings.TrimPrefix(req.URL.Path, "/")

	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	// api.github.com paths start with /repos/{owner}/{repo}/...
	if strings.Contains(host, "api.github.com") {
		path = strings.TrimPrefix(path, "repos/")
	}

	for _, repo := range p.repos {
		if path == repo || strings.HasPrefix(path, repo+"/") || strings.HasPrefix(path, repo+".git") {
			return true
		}
	}
	return false
}

func (p *githubAppProvider) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)

	// Parse secret reference. Supports "sm://project/secret" format.
	secretName := p.secretRef
	gcpProject := p.gcpProject
	if rest, ok := strings.CutPrefix(secretName, "sm://"); ok {
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 {
			gcpProject = parts[0]
			secretName = parts[1]
		}
	}

	client, err := github.NewTokenClient(ctx, p.appID, secretName, gcpProject)
	if err != nil {
		return fmt.Errorf("creating GitHub token client: %w", err)
	}
	p.client = client

	// Fetch initial token.
	if err := p.refreshToken(); err != nil {
		p.logger.WithError(err).Warn("Initial GitHub token fetch failed (will retry)")
	}

	go p.refreshLoop()
	return nil
}

func (p *githubAppProvider) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *githubAppProvider) getToken() (string, error) {
	p.mu.RLock()
	tok := p.token
	exp := p.expiresAt
	p.mu.RUnlock()

	if tok != "" && time.Now().Before(exp) {
		return tok, nil
	}

	// Synchronous refresh.
	if err := p.refreshToken(); err != nil {
		return "", err
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.token, nil
}

func (p *githubAppProvider) refreshToken() error {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	token, err := p.client.GetInstallationToken(ctx)
	if err != nil {
		return fmt.Errorf("getting installation token: %w", err)
	}

	// GitHub App installation tokens expire in 1 hour.
	p.mu.Lock()
	p.token = token
	p.expiresAt = time.Now().Add(55 * time.Minute) // 5min safety margin
	p.mu.Unlock()

	p.logger.Debug("Refreshed GitHub App installation token")
	return nil
}

func (p *githubAppProvider) refreshLoop() {
	for {
		// Refresh at 45 minutes (installation tokens expire in 60 min).
		select {
		case <-time.After(45 * time.Minute):
			if err := p.refreshToken(); err != nil {
				p.logger.WithError(err).Warn("GitHub token refresh failed")
			}
		case <-p.ctx.Done():
			return
		}
	}
}
