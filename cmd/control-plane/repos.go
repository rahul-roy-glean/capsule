package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/repo"
)

// Repo represents a managed repository
type Repo struct {
	Slug                 string    `json:"slug"`
	URL                  string    `json:"url"`
	Branch               string    `json:"branch"`
	BazelVersion         string    `json:"bazel_version"`
	WarmupTargets        string    `json:"warmup_targets"`
	BuildSchedule        string    `json:"build_schedule"`
	MaxConcurrentRunners int       `json:"max_concurrent_runners"`
	CurrentVersion       string    `json:"current_version"`
	AutoRollout          bool      `json:"auto_rollout"`
	CreatedAt            time.Time `json:"created_at"`
}

// RepoRegistry manages the repos table
type RepoRegistry struct {
	db     *sql.DB
	logger *logrus.Entry
}

// NewRepoRegistry creates a new RepoRegistry
func NewRepoRegistry(db *sql.DB, logger *logrus.Logger) *RepoRegistry {
	return &RepoRegistry{
		db:     db,
		logger: logger.WithField("component", "repo-registry"),
	}
}

// RegisterRepo registers a new repository
func (rr *RepoRegistry) RegisterRepo(ctx context.Context, repoURL, branch, bazelVersion, warmupTargets, buildSchedule string, maxConcurrent int) (*Repo, error) {
	slug := repo.Slug(repoURL)
	if branch == "" {
		branch = "main"
	}
	if warmupTargets == "" {
		warmupTargets = "//..."
	}

	rr.logger.WithFields(logrus.Fields{
		"slug":   slug,
		"url":    repoURL,
		"branch": branch,
	}).Info("Registering repository")

	_, err := rr.db.ExecContext(ctx, `
		INSERT INTO repos (slug, url, branch, bazel_version, warmup_targets, build_schedule, max_concurrent_runners)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (slug) DO UPDATE SET
			url = EXCLUDED.url,
			branch = EXCLUDED.branch,
			bazel_version = EXCLUDED.bazel_version,
			warmup_targets = EXCLUDED.warmup_targets,
			build_schedule = EXCLUDED.build_schedule,
			max_concurrent_runners = EXCLUDED.max_concurrent_runners
	`, slug, repoURL, branch, bazelVersion, warmupTargets, buildSchedule, maxConcurrent)
	if err != nil {
		return nil, fmt.Errorf("failed to register repo: %w", err)
	}

	return rr.GetRepo(ctx, slug)
}

// GetRepo returns a repo by slug
func (rr *RepoRegistry) GetRepo(ctx context.Context, slug string) (*Repo, error) {
	var r Repo
	var currentVersion sql.NullString

	err := rr.db.QueryRowContext(ctx, `
		SELECT slug, url, branch, bazel_version, warmup_targets, build_schedule,
		       max_concurrent_runners, current_version, auto_rollout, created_at
		FROM repos WHERE slug = $1
	`, slug).Scan(&r.Slug, &r.URL, &r.Branch, &r.BazelVersion, &r.WarmupTargets,
		&r.BuildSchedule, &r.MaxConcurrentRunners, &currentVersion, &r.AutoRollout, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("repo not found: %s", slug)
	}
	if err != nil {
		return nil, err
	}

	if currentVersion.Valid {
		r.CurrentVersion = currentVersion.String
	}

	return &r, nil
}

// ListRepos returns all registered repos
func (rr *RepoRegistry) ListRepos(ctx context.Context) ([]*Repo, error) {
	rows, err := rr.db.QueryContext(ctx, `
		SELECT slug, url, branch, bazel_version, warmup_targets, build_schedule,
		       max_concurrent_runners, current_version, auto_rollout, created_at
		FROM repos ORDER BY slug
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []*Repo
	for rows.Next() {
		var r Repo
		var currentVersion sql.NullString

		if err := rows.Scan(&r.Slug, &r.URL, &r.Branch, &r.BazelVersion, &r.WarmupTargets,
			&r.BuildSchedule, &r.MaxConcurrentRunners, &currentVersion, &r.AutoRollout, &r.CreatedAt); err != nil {
			return nil, err
		}
		if currentVersion.Valid {
			r.CurrentVersion = currentVersion.String
		}
		repos = append(repos, &r)
	}

	return repos, nil
}

// UpdateRepo updates a repository's configuration
func (rr *RepoRegistry) UpdateRepo(ctx context.Context, slug string, updates map[string]interface{}) error {
	var sets []string
	var args []interface{}
	argIdx := 1

	for key, value := range updates {
		// Whitelist allowed fields
		switch key {
		case "branch", "bazel_version", "warmup_targets", "build_schedule", "current_version":
			sets = append(sets, fmt.Sprintf("%s = $%d", key, argIdx))
			args = append(args, value)
			argIdx++
		case "max_concurrent_runners":
			sets = append(sets, fmt.Sprintf("%s = $%d", key, argIdx))
			args = append(args, value)
			argIdx++
		case "auto_rollout":
			sets = append(sets, fmt.Sprintf("%s = $%d", key, argIdx))
			args = append(args, value)
			argIdx++
		default:
			return fmt.Errorf("unknown field: %s", key)
		}
	}

	if len(sets) == 0 {
		return nil
	}

	args = append(args, slug)
	query := fmt.Sprintf("UPDATE repos SET %s WHERE slug = $%d", strings.Join(sets, ", "), argIdx)

	result, err := rr.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("repo not found: %s", slug)
	}

	return nil
}

// AdoptSnapshot tags an existing snapshot (built without a repo slug) so it
// belongs to this repo. This is used during migration from single-repo to
// multi-repo: the old snapshot at gs://bucket/<version>/ stays where it is,
// but its DB record gets repo and repo_slug set so the system can find it.
// The GCS files are NOT moved — hosts can still load from the old path.
func (rr *RepoRegistry) AdoptSnapshot(ctx context.Context, slug, version string) error {
	result, err := rr.db.ExecContext(ctx, `
		UPDATE snapshots SET repo_slug = $1 WHERE version = $2 AND (repo_slug = '' OR repo_slug IS NULL)
	`, slug, version)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("snapshot %s not found or already owned by a repo", version)
	}

	// Also set it as current_version on the repo
	_, err = rr.db.ExecContext(ctx, `UPDATE repos SET current_version = $2 WHERE slug = $1`, slug, version)
	return err
}

// GetCurrentVersion returns the current active version for a repo
func (rr *RepoRegistry) GetCurrentVersion(ctx context.Context, repoSlug string) (string, error) {
	var version sql.NullString
	err := rr.db.QueryRowContext(ctx, `
		SELECT current_version FROM repos WHERE slug = $1
	`, repoSlug).Scan(&version)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("repo not found: %s", repoSlug)
	}
	if err != nil {
		return "", err
	}
	if !version.Valid {
		return "", nil
	}
	return version.String, nil
}

// HTTP Handlers

// HandleListRepos handles GET /api/v1/repos
func (rr *RepoRegistry) HandleListRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	repos, err := rr.ListRepos(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"repos": repos,
		"count": len(repos),
	})
}

// HandleCreateRepo handles POST /api/v1/repos
func (rr *RepoRegistry) HandleCreateRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		URL                  string `json:"url"`
		Branch               string `json:"branch"`
		BazelVersion         string `json:"bazel_version"`
		WarmupTargets        string `json:"warmup_targets"`
		BuildSchedule        string `json:"build_schedule"`
		MaxConcurrentRunners int    `json:"max_concurrent_runners"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}

	rp, err := rr.RegisterRepo(r.Context(), req.URL, req.Branch, req.BazelVersion,
		req.WarmupTargets, req.BuildSchedule, req.MaxConcurrentRunners)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rp)
}

// HandleGetRepo handles GET /api/v1/repos/{slug}
func (rr *RepoRegistry) HandleGetRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	slug := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	rp, err := rr.GetRepo(r.Context(), slug)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rp)
}

// HandleUpdateRepo handles PUT /api/v1/repos/{slug}
func (rr *RepoRegistry) HandleUpdateRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	slug := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := rr.UpdateRepo(r.Context(), slug, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	rp, err := rr.GetRepo(r.Context(), slug)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rp)
}

// HandleRepos is a multiplexer for /api/v1/repos endpoints
func (rr *RepoRegistry) HandleRepos(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/repos")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		// /api/v1/repos
		switch r.Method {
		case http.MethodGet:
			rr.HandleListRepos(w, r)
		case http.MethodPost:
			rr.HandleCreateRepo(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// /api/v1/repos/{slug}
	switch r.Method {
	case http.MethodGet:
		rr.HandleGetRepo(w, r)
	case http.MethodPut:
		rr.HandleUpdateRepo(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
