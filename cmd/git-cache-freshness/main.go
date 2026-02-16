package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

var (
	// Source: GCS bucket or GCP snapshot
	gcsBucket      = flag.String("gcs-bucket", "", "GCS bucket containing git-cache metadata (legacy mode)")
	gcpProject     = flag.String("gcp-project", "", "GCP project for snapshot/Cloud Build")
	snapshotPrefix = flag.String("snapshot-prefix", "runner-data", "Prefix for snapshot names")

	// Freshness thresholds
	maxAgeHours    = flag.Int("max-age-hours", 24, "Maximum age before rebuild (hours)")
	maxCommitDrift = flag.Int("max-commit-drift", 50, "Maximum commits behind before rebuild")

	// GitHub
	githubToken = flag.String("github-token", "", "GitHub token for API access (or GITHUB_TOKEN env)")

	// Rebuild trigger
	triggerRebuild    = flag.Bool("trigger-rebuild", false, "Trigger Cloud Build rebuild if stale")
	cloudBuildTrigger = flag.String("cloud-build-trigger", "", "Cloud Build trigger ID for rebuild")

	logLevel = flag.String("log-level", "info", "Log level")
)

// GitCacheMetadata from git-cache-builder
type GitCacheMetadata struct {
	Version   string                `json:"version"`
	BuildTime time.Time             `json:"build_time"`
	Repos     map[string]RepoStatus `json:"repos"`
	ImageSize int64                 `json:"image_size_bytes"`
}

type RepoStatus struct {
	URL       string    `json:"url"`
	Branch    string    `json:"branch"`
	CommitSHA string    `json:"commit_sha"`
	ClonedAt  time.Time `json:"cloned_at"`
}

// FreshnessReport summarizes the check results
type FreshnessReport struct {
	CheckTime        time.Time                `json:"check_time"`
	CacheVersion     string                   `json:"cache_version"`
	CacheAge         string                   `json:"cache_age"`
	CacheAgeHours    float64                  `json:"cache_age_hours"`
	IsStale          bool                     `json:"is_stale"`
	StaleReason      string                   `json:"stale_reason,omitempty"`
	RepoStatus       map[string]RepoFreshness `json:"repo_status"`
	RebuildTriggered bool                     `json:"rebuild_triggered,omitempty"`
}

type RepoFreshness struct {
	URL           string `json:"url"`
	CachedSHA     string `json:"cached_sha"`
	CurrentSHA    string `json:"current_sha"`
	CommitsBehind int    `json:"commits_behind"`
	IsStale       bool   `json:"is_stale"`
	Error         string `json:"error,omitempty"`
}

func main() {
	flag.Parse()

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	level, _ := logrus.ParseLevel(*logLevel)
	logger.SetLevel(level)

	log := logger.WithField("component", "git-cache-freshness")

	if *gcsBucket == "" {
		log.Fatal("--gcs-bucket is required")
	}

	token := *githubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}

	ctx := context.Background()

	// Download metadata from GCS
	metadata, err := downloadMetadata(ctx, *gcsBucket)
	if err != nil {
		log.WithError(err).Fatal("Failed to download git-cache metadata")
	}

	log.WithFields(logrus.Fields{
		"version":    metadata.Version,
		"build_time": metadata.BuildTime,
		"repos":      len(metadata.Repos),
	}).Info("Loaded git-cache metadata")

	// Check freshness
	report := checkFreshness(ctx, metadata, token, *maxAgeHours, *maxCommitDrift, log)

	// Output report
	reportJSON, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(reportJSON))

	// Trigger rebuild if needed
	if report.IsStale && *triggerRebuild && *cloudBuildTrigger != "" && *gcpProject != "" {
		log.WithField("reason", report.StaleReason).Info("Git-cache is stale, triggering rebuild")

		if err := triggerCloudBuild(ctx, *gcpProject, *cloudBuildTrigger, log); err != nil {
			log.WithError(err).Error("Failed to trigger Cloud Build")
			os.Exit(1)
		}
		report.RebuildTriggered = true
		log.Info("Cloud Build rebuild triggered")
	}

	if report.IsStale {
		os.Exit(1) // Exit non-zero to signal staleness to CI/cron
	}
}

func downloadMetadata(ctx context.Context, bucket string) (*GitCacheMetadata, error) {
	gcsPath := fmt.Sprintf("gs://%s/git-cache/current/metadata.json", bucket)

	// Download to temp file
	tmpFile, err := os.CreateTemp("", "git-cache-metadata-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	cmd := exec.CommandContext(ctx, "gcloud", "storage", "cp", gcsPath, tmpFile.Name())
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gcloud storage cp failed: %s: %w", string(output), err)
	}

	data, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		return nil, err
	}

	var metadata GitCacheMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}

func checkFreshness(ctx context.Context, metadata *GitCacheMetadata, token string, maxAgeHours, maxCommitDrift int, log *logrus.Entry) *FreshnessReport {
	report := &FreshnessReport{
		CheckTime:     time.Now(),
		CacheVersion:  metadata.Version,
		CacheAge:      time.Since(metadata.BuildTime).String(),
		CacheAgeHours: time.Since(metadata.BuildTime).Hours(),
		RepoStatus:    make(map[string]RepoFreshness),
	}

	// Check age
	if report.CacheAgeHours > float64(maxAgeHours) {
		report.IsStale = true
		report.StaleReason = fmt.Sprintf("Cache is %.1f hours old (max: %d)", report.CacheAgeHours, maxAgeHours)
	}

	// Check each repo for commit drift
	for name, status := range metadata.Repos {
		freshness := checkRepoFreshness(ctx, status, token, maxCommitDrift, log)
		report.RepoStatus[name] = freshness

		if freshness.IsStale && !report.IsStale {
			report.IsStale = true
			report.StaleReason = fmt.Sprintf("Repo %s is %d commits behind (max: %d)",
				name, freshness.CommitsBehind, maxCommitDrift)
		}
	}

	return report
}

func checkRepoFreshness(ctx context.Context, status RepoStatus, token string, maxDrift int, log *logrus.Entry) RepoFreshness {
	freshness := RepoFreshness{
		URL:       status.URL,
		CachedSHA: status.CommitSHA,
	}

	// Parse repo URL to get owner/repo
	// github.com/askscio/scio -> askscio/scio
	repoPath := strings.TrimPrefix(status.URL, "github.com/")
	if repoPath == status.URL {
		freshness.Error = "Non-GitHub URL, skipping"
		return freshness
	}

	// Get current HEAD from GitHub API
	branch := status.Branch
	if branch == "" {
		branch = "main"
	}

	currentSHA, commitsBehind, err := getCommitInfo(ctx, repoPath, branch, status.CommitSHA, token)
	if err != nil {
		freshness.Error = err.Error()
		return freshness
	}

	freshness.CurrentSHA = currentSHA
	freshness.CommitsBehind = commitsBehind
	freshness.IsStale = commitsBehind > maxDrift

	return freshness
}

func getCommitInfo(ctx context.Context, repo, branch, cachedSHA, token string) (currentSHA string, commitsBehind int, err error) {
	// Get current branch HEAD
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits/%s", repo, branch)

	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commit); err != nil {
		return "", 0, err
	}

	currentSHA = commit.SHA

	if currentSHA == cachedSHA {
		return currentSHA, 0, nil
	}

	// Compare commits to get count
	compareURL := fmt.Sprintf("https://api.github.com/repos/%s/compare/%s...%s", repo, cachedSHA[:12], currentSHA[:12])

	req, _ = http.NewRequestWithContext(ctx, "GET", compareURL, nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err = client.Do(req)
	if err != nil {
		// Fall back to assuming it's stale if we can't compare
		return currentSHA, 999, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return currentSHA, 999, nil
	}

	var compare struct {
		AheadBy  int `json:"ahead_by"`
		BehindBy int `json:"behind_by"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&compare); err != nil {
		return currentSHA, 999, nil
	}

	return currentSHA, compare.AheadBy, nil
}

func triggerCloudBuild(ctx context.Context, project, triggerID string, log *logrus.Entry) error {
	cmd := exec.CommandContext(ctx, "gcloud", "builds", "triggers", "run", triggerID,
		"--project", project,
		"--branch", "main")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gcloud builds triggers run failed: %s: %w", string(output), err)
	}

	log.WithField("output", string(output)).Debug("Cloud Build trigger output")
	return nil
}
