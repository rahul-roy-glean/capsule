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
)

// SnapshotTag represents a named version tag for a snapshot config.
type SnapshotTag struct {
	Tag         string    `json:"tag"`
	WorkloadKey string    `json:"workload_key"`
	Version     string    `json:"version"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// SnapshotTagRegistry manages the snapshot_tags table.
type SnapshotTagRegistry struct {
	db     *sql.DB
	logger *logrus.Entry
}

// NewSnapshotTagRegistry creates a new SnapshotTagRegistry.
func NewSnapshotTagRegistry(db *sql.DB, logger *logrus.Logger) *SnapshotTagRegistry {
	return &SnapshotTagRegistry{
		db:     db,
		logger: logger.WithField("component", "snapshot-tag-registry"),
	}
}

// CreateOrUpdateTag upserts a snapshot tag.
func (r *SnapshotTagRegistry) CreateOrUpdateTag(ctx context.Context, workloadKey, tag, version, description string) (*SnapshotTag, error) {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO snapshot_tags (tag, workload_key, version, description)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tag, workload_key) DO UPDATE SET
			version = EXCLUDED.version,
			description = EXCLUDED.description
	`, tag, workloadKey, version, description)
	if err != nil {
		return nil, fmt.Errorf("failed to upsert snapshot tag: %w", err)
	}
	return r.GetTag(ctx, workloadKey, tag)
}

// GetTag returns a specific tag for a workload key.
func (r *SnapshotTagRegistry) GetTag(ctx context.Context, workloadKey, tag string) (*SnapshotTag, error) {
	var st SnapshotTag
	err := r.db.QueryRowContext(ctx, `
		SELECT tag, workload_key, version, description, created_at
		FROM snapshot_tags WHERE workload_key = $1 AND tag = $2
	`, workloadKey, tag).Scan(&st.Tag, &st.WorkloadKey, &st.Version, &st.Description, &st.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("tag not found: %s/%s", workloadKey, tag)
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// ListTags returns all tags for a workload key.
func (r *SnapshotTagRegistry) ListTags(ctx context.Context, workloadKey string) ([]*SnapshotTag, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT tag, workload_key, version, description, created_at
		FROM snapshot_tags WHERE workload_key = $1 ORDER BY tag
	`, workloadKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []*SnapshotTag
	for rows.Next() {
		var st SnapshotTag
		if err := rows.Scan(&st.Tag, &st.WorkloadKey, &st.Version, &st.Description, &st.CreatedAt); err != nil {
			return nil, err
		}
		tags = append(tags, &st)
	}
	return tags, nil
}

// DeleteTag deletes a tag.
func (r *SnapshotTagRegistry) DeleteTag(ctx context.Context, workloadKey, tag string) error {
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM snapshot_tags WHERE workload_key = $1 AND tag = $2
	`, workloadKey, tag)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("tag not found: %s/%s", workloadKey, tag)
	}
	return nil
}

// ResolveTagVersion looks up the version for a given tag and workload key.
func (r *SnapshotTagRegistry) ResolveTagVersion(ctx context.Context, workloadKey, tag string) (string, error) {
	var version string
	err := r.db.QueryRowContext(ctx, `
		SELECT version FROM snapshot_tags WHERE workload_key = $1 AND tag = $2
	`, workloadKey, tag).Scan(&version)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("tag not found: %s/%s", workloadKey, tag)
	}
	if err != nil {
		return "", err
	}
	return version, nil
}

// PromoteTag sets current_version on the snapshot config from the tagged version.
func (r *SnapshotTagRegistry) PromoteTag(ctx context.Context, workloadKey, tag string) (string, error) {
	version, err := r.ResolveTagVersion(ctx, workloadKey, tag)
	if err != nil {
		return "", err
	}
	_, err = r.db.ExecContext(ctx, `
		UPDATE snapshot_layers SET current_version = $1, updated_at = NOW()
		WHERE layer_hash = (SELECT leaf_layer_hash FROM layered_configs WHERE leaf_workload_key = $2 LIMIT 1)
	`, version, workloadKey)
	if err != nil {
		return "", fmt.Errorf("failed to promote tag: %w", err)
	}
	return version, nil
}

// HTTP Handlers

// HandleTags handles /api/v1/layered-configs/{wk}/tags and /api/v1/layered-configs/{wk}/tags/{tag}
func (r *SnapshotTagRegistry) HandleTags(w http.ResponseWriter, req *http.Request, workloadKey, subpath string) {
	// subpath is everything after "tags", e.g. "" or "/{tag}"
	tag := strings.TrimPrefix(subpath, "/")

	if tag == "" {
		// /tags
		switch req.Method {
		case http.MethodGet:
			r.handleListTags(w, req, workloadKey)
		case http.MethodPost:
			r.handleCreateTag(w, req, workloadKey)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// /tags/{tag}
	switch req.Method {
	case http.MethodGet:
		r.handleGetTag(w, req, workloadKey, tag)
	case http.MethodDelete:
		r.handleDeleteTag(w, req, workloadKey, tag)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandlePromote handles POST /api/v1/layered-configs/{wk}/promote
func (r *SnapshotTagRegistry) HandlePromote(w http.ResponseWriter, req *http.Request, workloadKey string) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if body.Tag == "" {
		http.Error(w, "tag is required", http.StatusBadRequest)
		return
	}
	version, err := r.PromoteTag(req.Context(), workloadKey, body.Tag)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"workload_key": workloadKey,
		"tag":          body.Tag,
		"version":      version,
		"status":       "promoted",
	})
}

func (r *SnapshotTagRegistry) handleListTags(w http.ResponseWriter, req *http.Request, workloadKey string) {
	tags, err := r.ListTags(req.Context(), workloadKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tags = nonNilSlice(tags)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"tags": tags, "count": len(tags)})
}

func (r *SnapshotTagRegistry) handleCreateTag(w http.ResponseWriter, req *http.Request, workloadKey string) {
	var body struct {
		Tag         string `json:"tag"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if body.Tag == "" || body.Version == "" {
		http.Error(w, "tag and version are required", http.StatusBadRequest)
		return
	}
	st, err := r.CreateOrUpdateTag(req.Context(), workloadKey, body.Tag, body.Version, body.Description)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(st)
}

func (r *SnapshotTagRegistry) handleGetTag(w http.ResponseWriter, req *http.Request, workloadKey, tag string) {
	st, err := r.GetTag(req.Context(), workloadKey, tag)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (r *SnapshotTagRegistry) handleDeleteTag(w http.ResponseWriter, req *http.Request, workloadKey, tag string) {
	if err := r.DeleteTag(req.Context(), workloadKey, tag); err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
