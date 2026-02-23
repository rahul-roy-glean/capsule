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

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// SnapshotConfig represents a named snapshot configuration keyed by chunk_key.
type SnapshotConfig struct {
	ChunkKey             string                    `json:"chunk_key"`
	DisplayName          string                    `json:"display_name"`
	Commands             []snapshot.SnapshotCommand `json:"commands"`
	BuildSchedule        string                    `json:"build_schedule"`
	MaxConcurrentRunners int                       `json:"max_concurrent_runners"`
	CurrentVersion       string                    `json:"current_version"`
	AutoRollout          bool                      `json:"auto_rollout"`
	CreatedAt            time.Time                 `json:"created_at"`
}

// SnapshotConfigRegistry manages the snapshot_configs table.
type SnapshotConfigRegistry struct {
	db     *sql.DB
	logger *logrus.Entry
}

// NewSnapshotConfigRegistry creates a new SnapshotConfigRegistry.
func NewSnapshotConfigRegistry(db *sql.DB, logger *logrus.Logger) *SnapshotConfigRegistry {
	return &SnapshotConfigRegistry{
		db:     db,
		logger: logger.WithField("component", "snapshot-config-registry"),
	}
}

// RegisterSnapshotConfig upserts a snapshot config, computing its chunk_key from commands.
func (r *SnapshotConfigRegistry) RegisterSnapshotConfig(ctx context.Context, displayName string, commands []snapshot.SnapshotCommand, buildSchedule string, maxConcurrent int) (*SnapshotConfig, error) {
	chunkKey := snapshot.ComputeChunkKey(commands)

	commandsJSON, err := json.Marshal(commands)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal commands: %w", err)
	}

	r.logger.WithFields(logrus.Fields{
		"chunk_key":    chunkKey,
		"display_name": displayName,
	}).Info("Registering snapshot config")

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO snapshot_configs (chunk_key, display_name, commands, build_schedule, max_concurrent_runners)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (chunk_key) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			commands = EXCLUDED.commands,
			build_schedule = EXCLUDED.build_schedule,
			max_concurrent_runners = EXCLUDED.max_concurrent_runners
	`, chunkKey, displayName, string(commandsJSON), buildSchedule, maxConcurrent)
	if err != nil {
		return nil, fmt.Errorf("failed to register snapshot config: %w", err)
	}

	return r.GetSnapshotConfig(ctx, chunkKey)
}

// GetSnapshotConfig returns a snapshot config by chunk_key.
func (r *SnapshotConfigRegistry) GetSnapshotConfig(ctx context.Context, chunkKey string) (*SnapshotConfig, error) {
	var sc SnapshotConfig
	var currentVersion sql.NullString
	var commandsJSON string

	err := r.db.QueryRowContext(ctx, `
		SELECT chunk_key, display_name, commands, build_schedule,
		       max_concurrent_runners, current_version, auto_rollout, created_at
		FROM snapshot_configs WHERE chunk_key = $1
	`, chunkKey).Scan(&sc.ChunkKey, &sc.DisplayName, &commandsJSON, &sc.BuildSchedule,
		&sc.MaxConcurrentRunners, &currentVersion, &sc.AutoRollout, &sc.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("snapshot config not found: %s", chunkKey)
	}
	if err != nil {
		return nil, err
	}
	if currentVersion.Valid {
		sc.CurrentVersion = currentVersion.String
	}
	if commandsJSON != "" {
		json.Unmarshal([]byte(commandsJSON), &sc.Commands)
	}
	return &sc, nil
}

// ListSnapshotConfigs returns all registered snapshot configs.
func (r *SnapshotConfigRegistry) ListSnapshotConfigs(ctx context.Context) ([]*SnapshotConfig, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT chunk_key, display_name, commands, build_schedule,
		       max_concurrent_runners, current_version, auto_rollout, created_at
		FROM snapshot_configs ORDER BY chunk_key
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []*SnapshotConfig
	for rows.Next() {
		var sc SnapshotConfig
		var currentVersion sql.NullString
		var commandsJSON string

		if err := rows.Scan(&sc.ChunkKey, &sc.DisplayName, &commandsJSON, &sc.BuildSchedule,
			&sc.MaxConcurrentRunners, &currentVersion, &sc.AutoRollout, &sc.CreatedAt); err != nil {
			return nil, err
		}
		if currentVersion.Valid {
			sc.CurrentVersion = currentVersion.String
		}
		if commandsJSON != "" {
			json.Unmarshal([]byte(commandsJSON), &sc.Commands)
		}
		configs = append(configs, &sc)
	}
	return configs, nil
}

// HTTP Handlers

// HandleListSnapshotConfigs handles GET /api/v1/snapshot-configs
func (r *SnapshotConfigRegistry) HandleListSnapshotConfigs(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	configs, err := r.ListSnapshotConfigs(req.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"configs": configs, "count": len(configs)})
}

// HandleCreateSnapshotConfig handles POST /api/v1/snapshot-configs
func (r *SnapshotConfigRegistry) HandleCreateSnapshotConfig(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		DisplayName          string                    `json:"display_name"`
		Commands             []snapshot.SnapshotCommand `json:"commands"`
		BuildSchedule        string                    `json:"build_schedule"`
		MaxConcurrentRunners int                       `json:"max_concurrent_runners"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if len(body.Commands) == 0 {
		http.Error(w, "commands is required and must be non-empty", http.StatusBadRequest)
		return
	}
	sc, err := r.RegisterSnapshotConfig(req.Context(), body.DisplayName, body.Commands, body.BuildSchedule, body.MaxConcurrentRunners)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(sc)
}

// HandleGetSnapshotConfig handles GET /api/v1/snapshot-configs/{chunk_key}
func (r *SnapshotConfigRegistry) HandleGetSnapshotConfig(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	chunkKey := strings.TrimPrefix(req.URL.Path, "/api/v1/snapshot-configs/")
	if chunkKey == "" {
		http.Error(w, "chunk_key is required", http.StatusBadRequest)
		return
	}
	sc, err := r.GetSnapshotConfig(req.Context(), chunkKey)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sc)
}

// HandleSnapshotConfigs is a multiplexer for /api/v1/snapshot-configs endpoints.
func (r *SnapshotConfigRegistry) HandleSnapshotConfigs(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/api/v1/snapshot-configs")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		switch req.Method {
		case http.MethodGet:
			r.HandleListSnapshotConfigs(w, req)
		case http.MethodPost:
			r.HandleCreateSnapshotConfig(w, req)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	switch req.Method {
	case http.MethodGet:
		r.HandleGetSnapshotConfig(w, req)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
