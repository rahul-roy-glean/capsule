package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// checkCommitDrift uses the GitHub API to count commits between a known commit
// and the HEAD of a branch. Returns the number of commits ahead.
func checkCommitDrift(ctx context.Context, repoURL, branch, lastCommit, githubToken string) (int, error) {
	// Extract owner/repo from URL
	owner, repoName, err := parseGitHubRepo(repoURL)
	if err != nil {
		return 0, fmt.Errorf("failed to parse repo URL: %w", err)
	}

	if lastCommit == "" {
		return 0, nil // No known commit to compare against
	}

	// GET /repos/{owner}/{repo}/compare/{base}...{head}
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/compare/%s...%s",
		owner, repoName, lastCommit, branch)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if githubToken != "" {
		req.Header.Set("Authorization", "token "+githubToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return -1, fmt.Errorf("commit %s not found (may have been force-pushed)", lastCommit)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var compareResp struct {
		AheadBy  int    `json:"ahead_by"`
		BehindBy int    `json:"behind_by"`
		Status   string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&compareResp); err != nil {
		return 0, fmt.Errorf("failed to parse compare response: %w", err)
	}

	return compareResp.AheadBy, nil
}

// parseGitHubRepo extracts owner and repo name from various GitHub URL formats.
func parseGitHubRepo(repoURL string) (owner, repoName string, err error) {
	s := repoURL

	// Strip schemes
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")

	// Strip github.com prefix
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimPrefix(s, "github.com:")

	// Strip .git suffix
	s = strings.TrimSuffix(s, ".git")

	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("cannot parse owner/repo from: %s", repoURL)
	}

	return parts[0], parts[1], nil
}

// snapshotFreshnessLoop iterates over all snapshot_configs and triggers rebuilds
// when a build_schedule fires or the snapshot is stale.
func snapshotFreshnessLoop(ctx context.Context, sm *SnapshotManager, configRegistry *SnapshotConfigRegistry, logger *logrus.Logger) {
	log := logger.WithField("component", "freshness-loop")
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkConfigFreshness(ctx, sm, configRegistry, log)
		}
	}
}

func checkConfigFreshness(ctx context.Context, sm *SnapshotManager, configRegistry *SnapshotConfigRegistry, log *logrus.Entry) {
	configs, err := configRegistry.ListSnapshotConfigs(ctx)
	if err != nil {
		log.WithError(err).Warn("Failed to list snapshot configs for freshness check")
		return
	}

	for _, cfg := range configs {
		if cfg.BuildSchedule == "" {
			continue // No scheduled builds for this config
		}

		if !shouldBuildNow(cfg.BuildSchedule) {
			continue
		}

		log := log.WithFields(logrus.Fields{
			"workload_key": cfg.WorkloadKey,
			"display_name": cfg.DisplayName,
		})

		// Check snapshot age
		currentSnapshot, err := sm.GetCurrentSnapshotForKey(ctx, cfg.WorkloadKey)
		if err != nil {
			log.WithError(err).Debug("No current snapshot for workload_key")
			log.Info("No snapshot exists for workload_key, triggering build")
			if _, err := sm.TriggerSnapshotBuildForKey(ctx, cfg.WorkloadKey, cfg.Commands, cfg.GitHubAppID, cfg.GitHubAppSecret, false); err != nil {
				log.WithError(err).Error("Failed to trigger snapshot build")
			}
			continue
		}

		age := time.Since(currentSnapshot.CreatedAt)
		if age > 24*time.Hour {
			log.WithFields(logrus.Fields{
				"version": currentSnapshot.Version,
				"age":     age,
			}).Info("Snapshot is stale (>24h), triggering rebuild")
			if _, err := sm.TriggerSnapshotBuildForKey(ctx, cfg.WorkloadKey, cfg.Commands, cfg.GitHubAppID, cfg.GitHubAppSecret, false); err != nil {
				log.WithError(err).Error("Failed to trigger snapshot build")
			}
			continue
		}

		// Check commit drift — only if commands contain a git-clone
		repoURL, branch := extractGitCloneArgs(cfg.Commands)
		if repoURL == "" {
			continue // No git-clone command, skip drift check
		}

		drift, err := checkCommitDrift(ctx, repoURL, branch, currentSnapshot.RepoCommit, "")
		if err != nil {
			log.WithError(err).Debug("Failed to check commit drift")
			continue
		}

		if drift > 0 {
			log.WithFields(logrus.Fields{
				"version":      currentSnapshot.Version,
				"commit_drift": drift,
			}).Info("Commit drift detected, triggering rebuild")
			if _, err := sm.TriggerSnapshotBuildForKey(ctx, cfg.WorkloadKey, cfg.Commands, cfg.GitHubAppID, cfg.GitHubAppSecret, false); err != nil {
				log.WithError(err).Error("Failed to trigger snapshot build")
			}
		}
	}
}

// extractGitCloneArgs returns the repo URL and branch from a git-clone command,
// or empty strings if no git-clone command is found.
func extractGitCloneArgs(commands []snapshot.SnapshotCommand) (repoURL, branch string) {
	for _, cmd := range commands {
		if cmd.Type == "git-clone" {
			// Args convention: ["<repo-url>", "<branch>"] or just ["<repo-url>"]
			if len(cmd.Args) >= 1 {
				repoURL = cmd.Args[0]
			}
			if len(cmd.Args) >= 2 {
				branch = cmd.Args[1]
			}
			if branch == "" {
				branch = "main"
			}
			return repoURL, branch
		}
	}
	return "", ""
}

// shouldBuildNow is a simplified cron check. It supports:
//   - "*/N * * * *" (every N minutes)
//   - Empty string (never)
//
// For production use, this should be replaced with a proper cron library.
func shouldBuildNow(schedule string) bool {
	if schedule == "" {
		return false
	}

	parts := strings.Fields(schedule)
	if len(parts) < 5 {
		return false
	}

	now := time.Now()

	// Simple minute-based matching for */N patterns
	minutePart := parts[0]
	if strings.HasPrefix(minutePart, "*/") {
		intervalStr := strings.TrimPrefix(minutePart, "*/")
		var interval int
		if _, err := fmt.Sscanf(intervalStr, "%d", &interval); err == nil && interval > 0 {
			return now.Minute()%interval == 0
		}
	}

	// For exact minute matches
	var minute int
	if _, err := fmt.Sscanf(minutePart, "%d", &minute); err == nil {
		return now.Minute() == minute
	}

	return false
}
