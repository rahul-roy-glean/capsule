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
)

func init() {
	authproxy.RegisterProvider("github-dual-token", newGitHubDualTokenProvider)
}

// githubDualTokenProvider injects one of two externally-pushed tokens (user or
// bot) depending on the GitHub API operation being performed.
//
// Bot token is used for code-review operations (POST to /comments, /reviews,
// /replies, /reactions). User token is used for everything else (git push/fetch,
// creating PRs, etc.).
//
// Both tokens are pushed via the /update-token endpoint with token_type "user"
// or "bot".
//
// Config keys:
//
//	repos              - comma-separated owner/repo list for repo-level isolation
//	bot_path_patterns  - comma-separated URL path substrings that trigger bot token
//	                     (default: "/comments,/reviews,/replies,/reactions")
type githubDualTokenProvider struct {
	hosts           []string
	repos           []string
	botPathPatterns []string

	mu          sync.RWMutex
	userToken   string
	userExpires time.Time
	botToken    string
	botExpires  time.Time

	logger *logrus.Entry
}

func newGitHubDualTokenProvider(cfg authproxy.ProviderConfig) (authproxy.CredentialProvider, error) {
	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("github-dual-token: at least one host is required")
	}

	var repos []string
	if r := cfg.Config["repos"]; r != "" {
		for _, repo := range strings.Split(r, ",") {
			repo = strings.TrimSpace(repo)
			if repo != "" {
				repos = append(repos, repo)
			}
		}
	}

	botPatterns := []string{"/comments", "/reviews", "/replies", "/reactions"}
	if p := cfg.Config["bot_path_patterns"]; p != "" {
		botPatterns = nil
		for _, pat := range strings.Split(p, ",") {
			pat = strings.TrimSpace(pat)
			if pat != "" {
				botPatterns = append(botPatterns, pat)
			}
		}
	}

	return &githubDualTokenProvider{
		hosts:           cfg.Hosts,
		repos:           repos,
		botPathPatterns: botPatterns,
		logger:          logrus.WithField("provider", "github-dual-token"),
	}, nil
}

func (p *githubDualTokenProvider) Name() string { return "github-dual-token" }

func (p *githubDualTokenProvider) Matches(host string) bool {
	return slices.Contains(p.hosts, host)
}

func (p *githubDualTokenProvider) InjectCredentials(req *http.Request) error {
	if len(p.repos) > 0 && !p.matchesRepo(req) {
		return nil
	}

	token, err := p.selectToken(req)
	if err != nil {
		return err
	}

	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if strings.Contains(host, "api.github.com") {
		req.Header.Set("Authorization", "token "+token)
	} else {
		req.SetBasicAuth("x-access-token", token)
	}
	return nil
}

func (p *githubDualTokenProvider) selectToken(req *http.Request) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.isBotOperation(req) {
		if p.botToken == "" {
			return "", fmt.Errorf("github-dual-token: bot token not available")
		}
		if !p.botExpires.IsZero() && time.Now().After(p.botExpires) {
			return "", fmt.Errorf("github-dual-token: bot token expired")
		}
		return p.botToken, nil
	}

	if p.userToken == "" {
		return "", fmt.Errorf("github-dual-token: user token not available")
	}
	if !p.userExpires.IsZero() && time.Now().After(p.userExpires) {
		return "", fmt.Errorf("github-dual-token: user token expired")
	}
	return p.userToken, nil
}

// isBotOperation returns true for POST requests whose path contains one of the
// configured bot path patterns (code-review operations).
func (p *githubDualTokenProvider) isBotOperation(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	path := req.URL.Path
	for _, pattern := range p.botPathPatterns {
		if strings.Contains(path, pattern) {
			return true
		}
	}
	return false
}

// matchesRepo checks if the request URL targets one of the configured repos.
// Matches patterns:
//   - github.com/{owner}/{repo}[/...]
//   - api.github.com/repos/{owner}/{repo}[/...]
func (p *githubDualTokenProvider) matchesRepo(req *http.Request) bool {
	path := strings.TrimPrefix(req.URL.Path, "/")

	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
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

func (p *githubDualTokenProvider) Start(_ context.Context) error { return nil }
func (p *githubDualTokenProvider) Stop()                         {}

// UpdateTypedToken implements authproxy.TypedTokenReceiver.
func (p *githubDualTokenProvider) UpdateTypedToken(tokenType, token string, expiresAt time.Time) error {
	if token == "" {
		return fmt.Errorf("github-dual-token: token must not be empty")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	switch tokenType {
	case "user":
		p.userToken = token
		p.userExpires = expiresAt
	case "bot":
		p.botToken = token
		p.botExpires = expiresAt
	default:
		return fmt.Errorf("github-dual-token: unknown token_type %q (expected \"user\" or \"bot\")", tokenType)
	}

	p.logger.WithFields(logrus.Fields{
		"token_type": tokenType,
		"expires_at": expiresAt.Format(time.RFC3339),
	}).Debug("Token updated")
	return nil
}

// UpdateToken implements authproxy.TokenReceiver for backward compatibility.
// An untyped token push is treated as a user token.
func (p *githubDualTokenProvider) UpdateToken(token string, expiresAt time.Time) error {
	return p.UpdateTypedToken("user", token, expiresAt)
}
