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
		// The commit might have been force-pushed away
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

// snapshotFreshnessLoop replaces the old FreshnessCheckLoop with a repo-aware version.
// For each registered repo with a build_schedule, it checks if the current snapshot
// is stale (by age or commit drift) and triggers a rebuild if needed.
func snapshotFreshnessLoop(ctx context.Context, sm *SnapshotManager, repoRegistry *RepoRegistry, logger *logrus.Logger) {
	log := logger.WithField("component", "freshness-loop")
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkRepoFreshness(ctx, sm, repoRegistry, log)
		}
	}
}

func checkRepoFreshness(ctx context.Context, sm *SnapshotManager, repoRegistry *RepoRegistry, log *logrus.Entry) {
	repos, err := repoRegistry.ListRepos(ctx)
	if err != nil {
		log.WithError(err).Warn("Failed to list repos for freshness check")
		return
	}

	for _, r := range repos {
		if r.BuildSchedule == "" {
			continue // No scheduled builds for this repo
		}

		// Check if current time matches the cron schedule
		if !shouldBuildNow(r.BuildSchedule) {
			continue
		}

		log := log.WithFields(logrus.Fields{
			"repo_slug": r.Slug,
			"repo_url":  r.URL,
		})

		// Check snapshot age
		currentSnapshot, err := sm.GetCurrentSnapshotForRepo(ctx, r.Slug)
		if err != nil {
			log.WithError(err).Debug("No current snapshot for repo")
			// No snapshot exists — trigger build
			log.Info("No snapshot exists for repo, triggering build")
			if _, err := sm.TriggerSnapshotBuildForRepo(ctx, r.Slug, r.URL, r.Branch, r.BazelVersion); err != nil {
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
			if _, err := sm.TriggerSnapshotBuildForRepo(ctx, r.Slug, r.URL, r.Branch, r.BazelVersion); err != nil {
				log.WithError(err).Error("Failed to trigger snapshot build")
			}
			continue
		}

		// Check commit drift
		drift, err := checkCommitDrift(ctx, r.URL, r.Branch, currentSnapshot.RepoCommit, "")
		if err != nil {
			log.WithError(err).Debug("Failed to check commit drift")
			continue
		}

		if drift > 0 {
			log.WithFields(logrus.Fields{
				"version":      currentSnapshot.Version,
				"commit_drift": drift,
			}).Info("Commit drift detected, triggering rebuild")
			if _, err := sm.TriggerSnapshotBuildForRepo(ctx, r.Slug, r.URL, r.Branch, r.BazelVersion); err != nil {
				log.WithError(err).Error("Failed to trigger snapshot build")
			}
		}
	}
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
