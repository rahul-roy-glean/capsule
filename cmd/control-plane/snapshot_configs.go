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
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/tiers"
)

// SnapshotConfig represents a named snapshot configuration keyed by workload_key.
type SnapshotConfig struct {
	WorkloadKey          string                     `json:"workload_key"`
	DisplayName          string                     `json:"display_name"`
	Commands             []snapshot.SnapshotCommand `json:"commands"`
	IncrementalCommands  []snapshot.SnapshotCommand `json:"incremental_commands,omitempty"`
	BuildSchedule        string                     `json:"build_schedule"`
	MaxConcurrentRunners int                        `json:"max_concurrent_runners"`
	CurrentVersion       string                     `json:"current_version"`
	AutoRollout          bool                       `json:"auto_rollout"`
	CISystem             string                     `json:"ci_system"`
	GitHubAppID          string                     `json:"github_app_id,omitempty"`
	GitHubAppSecret      string                     `json:"github_app_secret,omitempty"`
	StartCommand         *snapshot.StartCommand     `json:"start_command,omitempty"`
	RunnerTTLSeconds     int                        `json:"runner_ttl_seconds"`
	SessionMaxAgeSeconds int                        `json:"session_max_age_seconds"`
	AutoPause            bool                       `json:"auto_pause"`
	Tier                 string                     `json:"tier"`
	NetworkPolicy        json.RawMessage            `json:"network_policy,omitempty"`
	NetworkPolicyPreset  string                     `json:"network_policy_preset,omitempty"`
	CreatedAt            time.Time                  `json:"created_at"`
}

// SnapshotConfigRegistry manages the snapshot_configs table.
type SnapshotConfigRegistry struct {
	db              *sql.DB
	snapshotManager *SnapshotManager
	tagRegistry     *SnapshotTagRegistry
	logger          *logrus.Entry
}

// NewSnapshotConfigRegistry creates a new SnapshotConfigRegistry.
func NewSnapshotConfigRegistry(db *sql.DB, sm *SnapshotManager, tr *SnapshotTagRegistry, logger *logrus.Logger) *SnapshotConfigRegistry {
	return &SnapshotConfigRegistry{
		db:              db,
		snapshotManager: sm,
		tagRegistry:     tr,
		logger:          logger.WithField("component", "snapshot-config-registry"),
	}
}

// RegisterSnapshotConfig upserts a snapshot config, computing its workload_key from commands.
func (r *SnapshotConfigRegistry) RegisterSnapshotConfig(ctx context.Context, displayName string, commands []snapshot.SnapshotCommand, incrementalCommands []snapshot.SnapshotCommand, buildSchedule string, maxConcurrent int, ciSystem, githubAppID, githubAppSecret string, startCommand *snapshot.StartCommand, runnerTTLSeconds int, sessionMaxAgeSeconds int, autoPause bool, tier string) (*SnapshotConfig, error) {
	// Validate and default tier
	if tier == "" {
		tier = tiers.DefaultTier
	}
	if _, err := tiers.Lookup(tier); err != nil {
		return nil, fmt.Errorf("invalid tier: %w", err)
	}
	workloadKey := snapshot.ComputeWorkloadKey(commands)

	commandsJSON, err := json.Marshal(commands)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal commands: %w", err)
	}

	incrementalCommandsJSON, err := json.Marshal(incrementalCommands)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal incremental_commands: %w", err)
	}

	var startCommandJSON string
	if startCommand != nil {
		b, err := json.Marshal(startCommand)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal start_command: %w", err)
		}
		startCommandJSON = string(b)
	}

	r.logger.WithFields(logrus.Fields{
		"workload_key": workloadKey,
		"display_name": displayName,
		"ci_system":    ciSystem,
	}).Info("Registering snapshot config")

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO snapshot_configs (workload_key, display_name, commands, incremental_commands, build_schedule, max_concurrent_runners, ci_system, github_app_id, github_app_secret, start_command, runner_ttl_seconds, session_max_age_seconds, auto_pause, tier)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (workload_key) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			commands = EXCLUDED.commands,
			incremental_commands = EXCLUDED.incremental_commands,
			build_schedule = EXCLUDED.build_schedule,
			max_concurrent_runners = EXCLUDED.max_concurrent_runners,
			ci_system = EXCLUDED.ci_system,
			github_app_id = EXCLUDED.github_app_id,
			github_app_secret = EXCLUDED.github_app_secret,
			start_command = EXCLUDED.start_command,
			runner_ttl_seconds = EXCLUDED.runner_ttl_seconds,
			session_max_age_seconds = EXCLUDED.session_max_age_seconds,
			auto_pause = EXCLUDED.auto_pause,
			tier = EXCLUDED.tier
	`, workloadKey, displayName, string(commandsJSON), string(incrementalCommandsJSON), buildSchedule, maxConcurrent, ciSystem, githubAppID, githubAppSecret, startCommandJSON, runnerTTLSeconds, sessionMaxAgeSeconds, autoPause, tier)
	if err != nil {
		return nil, fmt.Errorf("failed to register snapshot config: %w", err)
	}

	return r.GetSnapshotConfig(ctx, workloadKey)
}

// GetSnapshotConfig returns a snapshot config by workload_key.
func (r *SnapshotConfigRegistry) GetSnapshotConfig(ctx context.Context, workloadKey string) (*SnapshotConfig, error) {
	var sc SnapshotConfig
	var currentVersion sql.NullString
	var commandsJSON string
	var githubAppID, githubAppSecret, startCommandJSON sql.NullString

	var incrementalCommandsJSON sql.NullString
	var networkPolicyJSON sql.NullString
	var networkPolicyPreset sql.NullString

	err := r.db.QueryRowContext(ctx, `
		SELECT workload_key, display_name, commands, incremental_commands, build_schedule,
		       max_concurrent_runners, current_version, auto_rollout,
		       ci_system, github_app_id, github_app_secret, start_command,
		       runner_ttl_seconds, session_max_age_seconds, auto_pause,
		       tier, network_policy, network_policy_preset, created_at
		FROM snapshot_configs WHERE workload_key = $1
	`, workloadKey).Scan(&sc.WorkloadKey, &sc.DisplayName, &commandsJSON, &incrementalCommandsJSON, &sc.BuildSchedule,
		&sc.MaxConcurrentRunners, &currentVersion, &sc.AutoRollout,
		&sc.CISystem, &githubAppID, &githubAppSecret, &startCommandJSON,
		&sc.RunnerTTLSeconds, &sc.SessionMaxAgeSeconds, &sc.AutoPause,
		&sc.Tier, &networkPolicyJSON, &networkPolicyPreset, &sc.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("snapshot config not found: %s", workloadKey)
	}
	if err != nil {
		return nil, err
	}
	if currentVersion.Valid {
		sc.CurrentVersion = currentVersion.String
	}
	if githubAppID.Valid {
		sc.GitHubAppID = githubAppID.String
	}
	if githubAppSecret.Valid {
		sc.GitHubAppSecret = githubAppSecret.String
	}
	if commandsJSON != "" {
		json.Unmarshal([]byte(commandsJSON), &sc.Commands)
	}
	if incrementalCommandsJSON.Valid && incrementalCommandsJSON.String != "" {
		json.Unmarshal([]byte(incrementalCommandsJSON.String), &sc.IncrementalCommands)
	}
	if startCommandJSON.Valid && startCommandJSON.String != "" {
		sc.StartCommand = &snapshot.StartCommand{}
		json.Unmarshal([]byte(startCommandJSON.String), sc.StartCommand)
	}
	if networkPolicyJSON.Valid && networkPolicyJSON.String != "" {
		sc.NetworkPolicy = json.RawMessage(networkPolicyJSON.String)
	}
	if networkPolicyPreset.Valid {
		sc.NetworkPolicyPreset = networkPolicyPreset.String
	}
	return &sc, nil
}

// ListSnapshotConfigs returns all registered snapshot configs.
func (r *SnapshotConfigRegistry) ListSnapshotConfigs(ctx context.Context) ([]*SnapshotConfig, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT workload_key, display_name, commands, incremental_commands, build_schedule,
		       max_concurrent_runners, current_version, auto_rollout,
		       ci_system, github_app_id, github_app_secret, start_command,
		       runner_ttl_seconds, session_max_age_seconds, auto_pause,
		       tier, network_policy, network_policy_preset, created_at
		FROM snapshot_configs ORDER BY workload_key
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
		var incrementalCommandsJSON sql.NullString
		var githubAppID, githubAppSecret, startCommandJSON sql.NullString
		var networkPolicyJSON, networkPolicyPreset sql.NullString

		if err := rows.Scan(&sc.WorkloadKey, &sc.DisplayName, &commandsJSON, &incrementalCommandsJSON, &sc.BuildSchedule,
			&sc.MaxConcurrentRunners, &currentVersion, &sc.AutoRollout,
			&sc.CISystem, &githubAppID, &githubAppSecret, &startCommandJSON,
			&sc.RunnerTTLSeconds, &sc.SessionMaxAgeSeconds, &sc.AutoPause,
			&sc.Tier, &networkPolicyJSON, &networkPolicyPreset, &sc.CreatedAt); err != nil {
			return nil, err
		}
		if currentVersion.Valid {
			sc.CurrentVersion = currentVersion.String
		}
		if githubAppID.Valid {
			sc.GitHubAppID = githubAppID.String
		}
		if githubAppSecret.Valid {
			sc.GitHubAppSecret = githubAppSecret.String
		}
		if commandsJSON != "" {
			json.Unmarshal([]byte(commandsJSON), &sc.Commands)
		}
		if incrementalCommandsJSON.Valid && incrementalCommandsJSON.String != "" {
			json.Unmarshal([]byte(incrementalCommandsJSON.String), &sc.IncrementalCommands)
		}
		if startCommandJSON.Valid && startCommandJSON.String != "" {
			sc.StartCommand = &snapshot.StartCommand{}
			json.Unmarshal([]byte(startCommandJSON.String), sc.StartCommand)
		}
		if networkPolicyJSON.Valid && networkPolicyJSON.String != "" {
			sc.NetworkPolicy = json.RawMessage(networkPolicyJSON.String)
		}
		if networkPolicyPreset.Valid {
			sc.NetworkPolicyPreset = networkPolicyPreset.String
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
		DisplayName          string                     `json:"display_name"`
		Commands             []snapshot.SnapshotCommand `json:"commands"`
		IncrementalCommands  []snapshot.SnapshotCommand `json:"incremental_commands"`
		BuildSchedule        string                     `json:"build_schedule"`
		MaxConcurrentRunners int                        `json:"max_concurrent_runners"`
		CISystem             string                     `json:"ci_system"`
		GitHubAppID          string                     `json:"github_app_id"`
		GitHubAppSecret      string                     `json:"github_app_secret"`
		StartCommand         *snapshot.StartCommand     `json:"start_command,omitempty"`
		RunnerTTLSeconds     int                        `json:"runner_ttl_seconds"`
		SessionMaxAgeSeconds int                        `json:"session_max_age_seconds"`
		AutoPause            bool                       `json:"auto_pause"`
		Tier                 string                     `json:"tier"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if len(body.Commands) == 0 {
		http.Error(w, "commands is required and must be non-empty", http.StatusBadRequest)
		return
	}
	sc, err := r.RegisterSnapshotConfig(req.Context(), body.DisplayName, body.Commands, body.IncrementalCommands, body.BuildSchedule, body.MaxConcurrentRunners, body.CISystem, body.GitHubAppID, body.GitHubAppSecret, body.StartCommand, body.RunnerTTLSeconds, body.SessionMaxAgeSeconds, body.AutoPause, body.Tier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(sc)
}

// HandleGetSnapshotConfig handles GET /api/v1/snapshot-configs/{workload_key}
func (r *SnapshotConfigRegistry) HandleGetSnapshotConfig(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	workloadKey := strings.TrimPrefix(req.URL.Path, "/api/v1/snapshot-configs/")
	if workloadKey == "" {
		http.Error(w, "workload_key is required", http.StatusBadRequest)
		return
	}
	sc, err := r.GetSnapshotConfig(req.Context(), workloadKey)
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

// HandleTriggerBuild handles POST /api/v1/snapshot-configs/{workload_key}/build
func (r *SnapshotConfigRegistry) HandleTriggerBuild(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract workload_key from path: .../snapshot-configs/{workload_key}/build
	path := strings.TrimPrefix(req.URL.Path, "/api/v1/snapshot-configs/")
	workloadKey := strings.TrimSuffix(path, "/build")
	if workloadKey == "" {
		http.Error(w, "workload_key is required", http.StatusBadRequest)
		return
	}

	sc, err := r.GetSnapshotConfig(req.Context(), workloadKey)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	incremental := req.URL.Query().Get("incremental") == "true"

	version, err := r.snapshotManager.TriggerSnapshotBuildForKey(req.Context(), sc.WorkloadKey, sc.Commands, sc.IncrementalCommands, sc.GitHubAppID, sc.GitHubAppSecret, incremental)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to trigger build: %s", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"workload_key": sc.WorkloadKey,
		"version":      version,
		"status":       "building",
		"incremental":  fmt.Sprintf("%v", incremental),
	})
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

	// Check for /build suffix
	if strings.HasSuffix(path, "/build") {
		r.HandleTriggerBuild(w, req)
		return
	}

	// Check for /tags or /tags/{tag} — route to tag registry
	if idx := strings.Index(path, "/tags"); idx >= 0 {
		workloadKey := path[:idx]
		subpath := path[idx+len("/tags"):]
		if r.tagRegistry != nil {
			r.tagRegistry.HandleTags(w, req, workloadKey, subpath)
		} else {
			http.Error(w, "tag registry not configured", http.StatusInternalServerError)
		}
		return
	}

	// Check for /promote — route to tag registry
	if strings.HasSuffix(path, "/promote") {
		workloadKey := strings.TrimSuffix(path, "/promote")
		if r.tagRegistry != nil {
			r.tagRegistry.HandlePromote(w, req, workloadKey)
		} else {
			http.Error(w, "tag registry not configured", http.StatusInternalServerError)
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
