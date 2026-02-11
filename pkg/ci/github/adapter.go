package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/ci"
	gh "github.com/rahul-roy-glean/bazel-firecracker/pkg/github"
)

// Config holds GitHub CI adapter configuration.
type Config struct {
	AppID      string
	AppSecret  string
	GCPProject string
	Repo       string
	Org        string
	Labels     []string
	Ephemeral  bool
}

// Adapter implements ci.Adapter for GitHub Actions.
type Adapter struct {
	config Config
	client *gh.TokenClient
	logger *logrus.Entry
}

var _ ci.Adapter = (*Adapter)(nil)

// NewAdapter creates a new GitHub CI adapter.
func NewAdapter(ctx context.Context, cfg Config, logger *logrus.Logger) (*Adapter, error) {
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf("github adapter requires app_id and app_secret")
	}

	client, err := gh.NewTokenClient(ctx, cfg.AppID, cfg.AppSecret, cfg.GCPProject)
	if err != nil {
		return nil, fmt.Errorf("failed to create github token client: %w", err)
	}

	return &Adapter{
		config: cfg,
		client: client,
		logger: logger.WithField("component", "ci-github"),
	}, nil
}

func (a *Adapter) Name() string { return "github-actions" }

func (a *Adapter) GetRunnerToken(ctx context.Context, opts ci.RunnerTokenOpts) (string, error) {
	org := opts.Org
	if org == "" {
		org = a.config.Org
	}
	repo := opts.Repo
	if repo == "" {
		repo = a.config.Repo
	}

	if org != "" {
		return a.client.GetOrgRunnerRegistrationToken(ctx, org)
	}
	if repo != "" {
		return a.client.GetRunnerRegistrationToken(ctx, repo)
	}
	return "", fmt.Errorf("no repo or org configured for GitHub runner registration")
}

func (a *Adapter) RunnerURL() string {
	if a.config.Org != "" {
		return fmt.Sprintf("https://github.com/%s", a.config.Org)
	}
	if a.config.Repo != "" {
		return fmt.Sprintf("https://github.com/%s", a.config.Repo)
	}
	return ""
}

func (a *Adapter) OnDrain(ctx context.Context, runners []ci.RunnerInfo) error {
	repo := a.config.Repo
	if repo == "" {
		a.logger.Debug("No GitHub repo configured, skipping drain label removal")
		return nil
	}

	for _, runner := range runners {
		name := runner.Name
		if len(name) > 8 {
			name = name[:8]
		}

		ghRunner, err := a.client.GetRunnerByName(ctx, repo, name)
		if err != nil {
			a.logger.WithError(err).WithField("runner_name", name).Debug("Runner not found in GitHub")
			continue
		}

		if err := a.client.RemoveAllCustomLabels(ctx, repo, ghRunner.ID); err != nil {
			a.logger.WithError(err).WithField("runner_name", name).Warn("Failed to remove labels")
			continue
		}

		a.logger.WithField("runner_name", name).Info("Removed labels from GitHub runner")
	}

	return nil
}

func (a *Adapter) OnRelease(ctx context.Context, runner ci.RunnerInfo) error {
	return nil
}

func (a *Adapter) WebhookHandler() http.Handler {
	return nil // Webhook is handled by the control-plane's existing handler
}

func (a *Adapter) Close() error {
	return nil
}

// GetClient returns the underlying GitHub token client for direct access if needed.
func (a *Adapter) GetClient() *gh.TokenClient {
	return a.client
}

// GetConfig returns the adapter configuration.
func (a *Adapter) GetConfig() Config {
	return a.config
}
