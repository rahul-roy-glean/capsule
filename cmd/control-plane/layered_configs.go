package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/tiers"
)

// LayeredConfigRegistry manages the layered_configs + snapshot_layers tables.
type LayeredConfigRegistry struct {
	db              *sql.DB
	snapshotManager *SnapshotManager
	layerBuilder    *LayerBuildScheduler
	configCache     *ConfigCache
	logger          *logrus.Entry
}

// NewLayeredConfigRegistry creates a new LayeredConfigRegistry.
func NewLayeredConfigRegistry(db *sql.DB, sm *SnapshotManager, logger *logrus.Logger) *LayeredConfigRegistry {
	return &LayeredConfigRegistry{
		db:              db,
		snapshotManager: sm,
		logger:          logger.WithField("component", "layered-config-registry"),
	}
}

// SetLayerBuilder sets the LayerBuildScheduler for triggering builds.
func (r *LayeredConfigRegistry) SetLayerBuilder(lb *LayerBuildScheduler) {
	r.layerBuilder = lb
}

// SetConfigCache sets the in-memory config cache for fast lookups.
func (r *LayeredConfigRegistry) SetConfigCache(cc *ConfigCache) {
	r.configCache = cc
}

// StoredLayeredConfig is the DB representation of a layered config.
type StoredLayeredConfig struct {
	ConfigID             string                 `json:"config_id"`
	DisplayName          string                 `json:"display_name"`
	LeafLayerHash        string                 `json:"leaf_layer_hash"`
	LeafWorkloadKey      string                 `json:"leaf_workload_key"`
	Tier                 string                 `json:"tier"`
	CISystem             string                 `json:"ci_system"`
	GitHubAppID          string                 `json:"github_app_id,omitempty"`
	GitHubAppSecret      string                 `json:"github_app_secret,omitempty"`
	StartCommand         *snapshot.StartCommand `json:"start_command,omitempty"`
	RunnerTTLSeconds     int                    `json:"runner_ttl_seconds"`
	SessionMaxAgeSeconds int                    `json:"session_max_age_seconds"`
	AutoPause            bool                   `json:"auto_pause"`
	AutoRollout          bool                   `json:"auto_rollout"`
	MaxConcurrentRunners int                    `json:"max_concurrent_runners"`
	BuildSchedule        string                 `json:"build_schedule"`
	NetworkPolicyPreset  string                 `json:"network_policy_preset,omitempty"`
	NetworkPolicy        json.RawMessage        `json:"network_policy,omitempty"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
}

// LayerStatus is the status of a single layer in a layered config.
type LayerStatus struct {
	Name           string `json:"name"`
	LayerHash      string `json:"layer_hash"`
	Status         string `json:"status"`
	CurrentVersion string `json:"current_version,omitempty"`
	Depth          int    `json:"depth"`
	BuildStatus    string `json:"build_status,omitempty"`
	BuildVersion   string `json:"build_version,omitempty"`
}

// RegisterLayeredConfig validates, materializes, and stores a layered config.
// It returns the config_id and leaf_workload_key.
func (r *LayeredConfigRegistry) RegisterLayeredConfig(ctx context.Context, cfg *snapshot.LayeredConfig) (configID, leafWorkloadKey string, err error) {
	if err := snapshot.ValidateLayeredConfig(cfg); err != nil {
		return "", "", fmt.Errorf("invalid config: %w", err)
	}

	// Validate tier
	tierName := cfg.Config.Tier
	if tierName == "" {
		tierName = tiers.DefaultTier
	}
	if _, err := tiers.Lookup(tierName); err != nil {
		return "", "", fmt.Errorf("invalid tier: %w", err)
	}

	layers := snapshot.MaterializeLayers(cfg)
	if len(layers) == 0 {
		return "", "", fmt.Errorf("no layers materialized")
	}

	leafLayer := layers[len(layers)-1]
	leafWorkloadKey = snapshot.ComputeLeafWorkloadKey(leafLayer.LayerHash)

	// Compute config_id from JSON
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal config: %w", err)
	}
	h := sha256.Sum256(cfgJSON)
	configID = hex.EncodeToString(h[:])[:16]

	r.logger.WithFields(logrus.Fields{
		"config_id":         configID,
		"display_name":      cfg.DisplayName,
		"leaf_workload_key": leafWorkloadKey,
		"num_layers":        len(layers),
	}).Info("Registering layered config")

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	// Insert layers in topological order (root first)
	for _, layer := range layers {
		initCmdsJSON, _ := json.Marshal(layer.InitCommands)
		refreshCmdsJSON, _ := json.Marshal(layer.RefreshCommands)
		drivesJSON, _ := json.Marshal(layer.Drives)
		allChainDrivesJSON, _ := json.Marshal(layer.AllChainDrives)

		var parentHash *string
		if layer.ParentLayerHash != "" {
			parentHash = &layer.ParentLayerHash
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO snapshot_layers (layer_hash, parent_layer_hash, config_name, depth, init_commands, refresh_commands, drives, all_chain_drives, refresh_interval)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (layer_hash) DO UPDATE SET
				config_name = EXCLUDED.config_name,
				refresh_commands = EXCLUDED.refresh_commands,
				refresh_interval = EXCLUDED.refresh_interval,
				all_chain_drives = EXCLUDED.all_chain_drives,
				status = CASE WHEN snapshot_layers.status = 'inactive' THEN 'pending' ELSE snapshot_layers.status END,
				current_version = CASE WHEN snapshot_layers.status = 'inactive' THEN NULL ELSE snapshot_layers.current_version END,
				updated_at = NOW()
		`, layer.LayerHash, parentHash, layer.Name, layer.Depth,
			string(initCmdsJSON), string(refreshCmdsJSON), string(drivesJSON), string(allChainDrivesJSON), layer.RefreshInterval)
		if err != nil {
			return "", "", fmt.Errorf("failed to insert layer %s: %w", layer.Name, err)
		}
	}

	// Marshal start_command
	var startCommandJSON string
	if cfg.StartCommand != nil {
		b, _ := json.Marshal(cfg.StartCommand)
		startCommandJSON = string(b)
	}

	// Upsert layered_configs
	_, err = tx.ExecContext(ctx, `
		INSERT INTO layered_configs (config_id, display_name, config_json, leaf_layer_hash, leaf_workload_key,
			tier, ci_system, github_app_id, github_app_secret, start_command,
			runner_ttl_seconds, session_max_age_seconds, auto_pause, auto_rollout,
			max_concurrent_runners, build_schedule, network_policy_preset, network_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (config_id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			config_json = EXCLUDED.config_json,
			leaf_layer_hash = EXCLUDED.leaf_layer_hash,
			leaf_workload_key = EXCLUDED.leaf_workload_key,
			tier = EXCLUDED.tier,
			ci_system = EXCLUDED.ci_system,
			github_app_id = EXCLUDED.github_app_id,
			github_app_secret = EXCLUDED.github_app_secret,
			start_command = EXCLUDED.start_command,
			runner_ttl_seconds = EXCLUDED.runner_ttl_seconds,
			session_max_age_seconds = EXCLUDED.session_max_age_seconds,
			auto_pause = EXCLUDED.auto_pause,
			auto_rollout = EXCLUDED.auto_rollout,
			max_concurrent_runners = EXCLUDED.max_concurrent_runners,
			build_schedule = EXCLUDED.build_schedule,
			network_policy_preset = EXCLUDED.network_policy_preset,
			network_policy = EXCLUDED.network_policy,
			updated_at = NOW()
	`, configID, cfg.DisplayName, string(cfgJSON), leafLayer.LayerHash, leafWorkloadKey,
		tierName, cfg.Config.CISystem, cfg.GitHubAppID, cfg.GitHubAppSecret, startCommandJSON,
		cfg.Config.TTL, cfg.Config.SessionMaxAgeSeconds, cfg.Config.AutoPause, cfg.Config.AutoRollout,
		0, "", cfg.Config.NetworkPolicyPreset, networkPolicyVal(cfg.Config.NetworkPolicy))

	if err != nil {
		return "", "", fmt.Errorf("failed to insert layered config: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", err
	}

	// Best-effort: populate repo_workload_mappings for CI webhook routing.
	// This is outside the transaction — a failed upsert doesn't break registration.
	for _, layer := range cfg.Layers {
		repoURL, _ := extractGitCloneArgs(layer.InitCommands)
		if repoURL != "" {
			if owner, repoName, parseErr := parseGitHubRepo(repoURL); parseErr == nil {
				repo := owner + "/" + repoName
				r.db.ExecContext(ctx, `
					INSERT INTO repo_workload_mappings (repo, workload_key) VALUES ($1, $2)
					ON CONFLICT (repo) DO UPDATE SET workload_key = EXCLUDED.workload_key
				`, repo, leafWorkloadKey)
				// Update in-memory cache
				if r.configCache != nil {
					r.configCache.PutRepoMapping(repo, leafWorkloadKey)
				}
			}
			break
		}
	}

	// Update in-memory workload config cache
	if r.configCache != nil {
		npJSON := ""
		if len(cfg.Config.NetworkPolicy) > 0 && string(cfg.Config.NetworkPolicy) != "null" {
			npJSON = string(cfg.Config.NetworkPolicy)
		}
		r.configCache.PutWorkloadConfig(&WorkloadConfig{
			WorkloadKey:          leafWorkloadKey,
			Tier:                 tierName,
			CISystem:             cfg.Config.CISystem,
			StartCommand:         cfg.StartCommand,
			RunnerTTLSeconds:     cfg.Config.TTL,
			SessionMaxAgeSeconds: cfg.Config.SessionMaxAgeSeconds,
			AutoPause:            cfg.Config.AutoPause,
			MaxConcurrentRunners: 0,
			NetworkPolicyPreset:  cfg.Config.NetworkPolicyPreset,
			NetworkPolicyJSON:    npJSON,
		})
	}

	return configID, leafWorkloadKey, nil
}

// GetLayeredConfig returns a stored layered config by config_id.
func (r *LayeredConfigRegistry) GetLayeredConfig(ctx context.Context, configID string) (*StoredLayeredConfig, error) {
	var sc StoredLayeredConfig
	var startCommandJSON sql.NullString
	var githubAppID, githubAppSecret sql.NullString
	var npPreset sql.NullString
	var npJSON sql.NullString

	err := r.db.QueryRowContext(ctx, `
		SELECT config_id, display_name, leaf_layer_hash, leaf_workload_key,
		       tier, ci_system, github_app_id, github_app_secret, start_command,
		       runner_ttl_seconds, session_max_age_seconds, auto_pause, auto_rollout,
		       max_concurrent_runners, build_schedule, network_policy_preset, network_policy,
		       created_at, updated_at
		FROM layered_configs WHERE config_id = $1
	`, configID).Scan(&sc.ConfigID, &sc.DisplayName, &sc.LeafLayerHash, &sc.LeafWorkloadKey,
		&sc.Tier, &sc.CISystem, &githubAppID, &githubAppSecret, &startCommandJSON,
		&sc.RunnerTTLSeconds, &sc.SessionMaxAgeSeconds, &sc.AutoPause, &sc.AutoRollout,
		&sc.MaxConcurrentRunners, &sc.BuildSchedule, &npPreset, &npJSON,
		&sc.CreatedAt, &sc.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("layered config not found: %s", configID)
	}
	if err != nil {
		return nil, err
	}
	if githubAppID.Valid {
		sc.GitHubAppID = githubAppID.String
	}
	if githubAppSecret.Valid {
		sc.GitHubAppSecret = githubAppSecret.String
	}
	if startCommandJSON.Valid && startCommandJSON.String != "" {
		sc.StartCommand = &snapshot.StartCommand{}
		json.Unmarshal([]byte(startCommandJSON.String), sc.StartCommand)
	}
	if npPreset.Valid {
		sc.NetworkPolicyPreset = npPreset.String
	}
	if npJSON.Valid && npJSON.String != "" && npJSON.String != "null" {
		sc.NetworkPolicy = json.RawMessage(npJSON.String)
	}
	return &sc, nil
}

// ListLayeredConfigs returns all layered configs.
func (r *LayeredConfigRegistry) ListLayeredConfigs(ctx context.Context) ([]*StoredLayeredConfig, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT config_id, display_name, leaf_layer_hash, leaf_workload_key,
		       tier, ci_system, github_app_id, github_app_secret, start_command,
		       runner_ttl_seconds, session_max_age_seconds, auto_pause, auto_rollout,
		       max_concurrent_runners, build_schedule, created_at, updated_at
		FROM layered_configs ORDER BY display_name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []*StoredLayeredConfig
	for rows.Next() {
		var sc StoredLayeredConfig
		var startCommandJSON, githubAppID, githubAppSecret sql.NullString

		if err := rows.Scan(&sc.ConfigID, &sc.DisplayName, &sc.LeafLayerHash, &sc.LeafWorkloadKey,
			&sc.Tier, &sc.CISystem, &githubAppID, &githubAppSecret, &startCommandJSON,
			&sc.RunnerTTLSeconds, &sc.SessionMaxAgeSeconds, &sc.AutoPause, &sc.AutoRollout,
			&sc.MaxConcurrentRunners, &sc.BuildSchedule, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
			return nil, err
		}
		if githubAppID.Valid {
			sc.GitHubAppID = githubAppID.String
		}
		if githubAppSecret.Valid {
			sc.GitHubAppSecret = githubAppSecret.String
		}
		if startCommandJSON.Valid && startCommandJSON.String != "" {
			sc.StartCommand = &snapshot.StartCommand{}
			json.Unmarshal([]byte(startCommandJSON.String), sc.StartCommand)
		}
		configs = append(configs, &sc)
	}
	return configs, nil
}

// GetLayerStatuses returns the status of all layers in a config.
// Walks up from leaf_layer_hash using parent pointers instead of
// re-parsing config JSON and re-computing hashes.
func (r *LayeredConfigRegistry) GetLayerStatuses(ctx context.Context, configID string) ([]LayerStatus, error) {
	var leafHash string
	err := r.db.QueryRowContext(ctx, `SELECT leaf_layer_hash FROM layered_configs WHERE config_id = $1`, configID).Scan(&leafHash)
	if err != nil {
		return nil, err
	}

	rows, err := r.db.QueryContext(ctx, `
		WITH RECURSIVE chain AS (
			SELECT layer_hash, parent_layer_hash, config_name, depth, status, current_version
			FROM snapshot_layers WHERE layer_hash = $1
			UNION ALL
			SELECT sl.layer_hash, sl.parent_layer_hash, sl.config_name, sl.depth, sl.status, sl.current_version
			FROM snapshot_layers sl
			JOIN chain c ON sl.layer_hash = c.parent_layer_hash
		)
		SELECT c.layer_hash, c.config_name, c.depth, c.status, c.current_version,
		       sb.status, sb.version
		FROM chain c
		LEFT JOIN snapshot_builds sb ON sb.layer_hash = c.layer_hash
			AND sb.status IN ('queued', 'waiting_parent', 'running')
		ORDER BY c.depth
	`, leafHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []LayerStatus
	for rows.Next() {
		var ls LayerStatus
		var status, currentVersion, buildStatus, buildVersion sql.NullString
		if err := rows.Scan(&ls.LayerHash, &ls.Name, &ls.Depth, &status, &currentVersion, &buildStatus, &buildVersion); err != nil {
			continue
		}
		if status.Valid {
			ls.Status = status.String
		} else {
			ls.Status = "unknown"
		}
		if currentVersion.Valid {
			ls.CurrentVersion = currentVersion.String
		}
		if buildStatus.Valid {
			ls.BuildStatus = buildStatus.String
		}
		if buildVersion.Valid {
			ls.BuildVersion = buildVersion.String
		}
		statuses = append(statuses, ls)
	}
	return statuses, nil
}

// DeleteLayeredConfig deletes a layered config. Layers shared by other configs are preserved;
// orphaned layers (not referenced by any remaining config) are deactivated and their builds cancelled.
func (r *LayeredConfigRegistry) DeleteLayeredConfig(ctx context.Context, configID string) error {
	// Load the config's layer hashes and workload key before deleting
	var configJSON, leafWorkloadKey string
	err := r.db.QueryRowContext(ctx, `SELECT config_json, leaf_workload_key FROM layered_configs WHERE config_id = $1`, configID).Scan(&configJSON, &leafWorkloadKey)
	if err != nil {
		return fmt.Errorf("layered config not found: %s", configID)
	}

	var cfg snapshot.LayeredConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}
	layers := snapshot.MaterializeLayers(&cfg)

	// Delete the config
	result, err := r.db.ExecContext(ctx, `DELETE FROM layered_configs WHERE config_id = $1`, configID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("layered config not found: %s", configID)
	}

	// Invalidate the workload config cache so the next allocation re-reads from DB.
	// If another config shares the same leaf_workload_key, the cache will be
	// repopulated on the next cache miss.
	if r.configCache != nil && leafWorkloadKey != "" {
		r.configCache.InvalidateWorkloadConfig(leafWorkloadKey)
	}

	// For each layer, check if it's still referenced by another config.
	// Walk the parent chain from each remaining config's leaf_layer_hash.
	// If the layer is in any config's chain, preserve it.
	for _, layer := range layers {
		var referenced int
		r.db.QueryRowContext(ctx, `
			WITH RECURSIVE config_layers AS (
				SELECT sl.layer_hash, sl.parent_layer_hash
				FROM snapshot_layers sl
				JOIN layered_configs lc ON lc.leaf_layer_hash = sl.layer_hash
				UNION ALL
				SELECT sl.layer_hash, sl.parent_layer_hash
				FROM snapshot_layers sl
				JOIN config_layers cl ON cl.parent_layer_hash = sl.layer_hash
			)
			SELECT COUNT(*) FROM config_layers WHERE layer_hash = $1
		`, layer.LayerHash).Scan(&referenced)

		if referenced > 0 {
			r.logger.WithFields(logrus.Fields{
				"layer_hash": layer.LayerHash[:16],
			}).Debug("Layer still referenced by other configs, preserving")
			continue
		}

		// Deactivate orphaned layer and clear current_version so re-registration starts fresh
		r.db.ExecContext(ctx, `UPDATE snapshot_layers SET status='inactive', current_version=NULL WHERE layer_hash=$1`, layer.LayerHash)

		// Cancel active builds
		r.db.ExecContext(ctx, `
			UPDATE snapshot_builds SET status='cancelled'
			WHERE layer_hash=$1 AND status IN ('queued','waiting_parent','running')
		`, layer.LayerHash)

		r.logger.WithFields(logrus.Fields{
			"layer_hash": layer.LayerHash[:16],
			"layer_name": layer.Name,
		}).Info("Deactivated orphaned layer and cancelled builds")
	}

	return nil
}

// LookupWorkloadKeyForRepo finds the leaf_workload_key matching the given repo name.
// Delegates to the shared lookupWorkloadKeyForRepo which uses indexed repo columns.
func (r *LayeredConfigRegistry) LookupWorkloadKeyForRepo(repoFullName string) string {
	return lookupWorkloadKeyForRepo(r.db, repoFullName)
}

// HTTP Handlers

// HandleLayeredConfigs is a multiplexer for /api/v1/layered-configs endpoints.
func (r *LayeredConfigRegistry) HandleLayeredConfigs(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/api/v1/layered-configs")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		switch req.Method {
		case http.MethodGet:
			r.handleListLayeredConfigs(w, req)
		case http.MethodPost:
			r.handleCreateLayeredConfig(w, req)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for /build suffix
	if strings.HasSuffix(path, "/build") {
		r.handleTriggerBuild(w, req)
		return
	}

	// Check for /layers/{layer_name}/refresh
	if strings.Contains(path, "/layers/") && strings.HasSuffix(path, "/refresh") {
		r.handleRefreshLayer(w, req)
		return
	}

	switch req.Method {
	case http.MethodGet:
		r.handleGetLayeredConfig(w, req)
	case http.MethodDelete:
		r.handleDeleteLayeredConfig(w, req)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *LayeredConfigRegistry) handleCreateLayeredConfig(w http.ResponseWriter, req *http.Request) {
	var cfg snapshot.LayeredConfig
	if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	configID, leafWorkloadKey, err := r.RegisterLayeredConfig(req.Context(), &cfg)
	if err != nil {
		if strings.Contains(err.Error(), "invalid") {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Get layer statuses for the response
	layers := snapshot.MaterializeLayers(&cfg)
	layerInfos := make([]map[string]interface{}, len(layers))
	for i, l := range layers {
		layerInfos[i] = map[string]interface{}{
			"name":   l.Name,
			"hash":   l.LayerHash,
			"depth":  l.Depth,
			"status": "pending",
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"config_id":         configID,
		"leaf_workload_key": leafWorkloadKey,
		"layers":            layerInfos,
	})
}

func (r *LayeredConfigRegistry) handleListLayeredConfigs(w http.ResponseWriter, req *http.Request) {
	configs, err := r.ListLayeredConfigs(req.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"configs": configs, "count": len(configs)})
}

func (r *LayeredConfigRegistry) handleGetLayeredConfig(w http.ResponseWriter, req *http.Request) {
	configID := strings.TrimPrefix(req.URL.Path, "/api/v1/layered-configs/")
	if configID == "" {
		http.Error(w, "config_id is required", http.StatusBadRequest)
		return
	}

	sc, err := r.GetLayeredConfig(req.Context(), configID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	layerStatuses, _ := r.GetLayerStatuses(req.Context(), configID)

	// Include the raw config definition so callers can see commands/layers
	var rawConfig json.RawMessage
	var cfgJSON string
	if err := r.db.QueryRowContext(req.Context(), `SELECT config_json FROM layered_configs WHERE config_id = $1`, configID).Scan(&cfgJSON); err == nil {
		rawConfig = json.RawMessage(cfgJSON)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"config":     sc,
		"layers":     layerStatuses,
		"definition": rawConfig,
	})
}

func (r *LayeredConfigRegistry) handleDeleteLayeredConfig(w http.ResponseWriter, req *http.Request) {
	configID := strings.TrimPrefix(req.URL.Path, "/api/v1/layered-configs/")
	if configID == "" {
		http.Error(w, "config_id is required", http.StatusBadRequest)
		return
	}

	if err := r.DeleteLayeredConfig(req.Context(), configID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (r *LayeredConfigRegistry) handleTriggerBuild(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(req.URL.Path, "/api/v1/layered-configs/")
	configID := strings.TrimSuffix(path, "/build")
	if configID == "" {
		http.Error(w, "config_id is required", http.StatusBadRequest)
		return
	}

	// Load config and materialize layers
	var cfgJSON string
	err := r.db.QueryRowContext(req.Context(), `SELECT config_json FROM layered_configs WHERE config_id = $1`, configID).Scan(&cfgJSON)
	if err != nil {
		http.Error(w, "config not found", http.StatusNotFound)
		return
	}

	var cfg snapshot.LayeredConfig
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		http.Error(w, "failed to parse stored config", http.StatusInternalServerError)
		return
	}

	layers := snapshot.MaterializeLayers(&cfg)
	forceRebuild := req.URL.Query().Get("force") == "true"
	cleanBuild := req.URL.Query().Get("clean") == "true"

	// clean=true: clear current_version for all layers so they rebuild from scratch (init)
	if cleanBuild {
		for _, layer := range layers {
			if _, err := r.db.ExecContext(req.Context(),
				`UPDATE snapshot_layers SET current_version=NULL WHERE layer_hash=$1`,
				layer.LayerHash); err != nil {
				logrus.WithError(err).WithField("layer_hash", layer.LayerHash[:16]).Error("Failed to clear layer version during clean build")
				http.Error(w, "failed to clear layer versions", http.StatusInternalServerError)
				return
			}
		}
	}

	if r.layerBuilder != nil {
		if err := r.layerBuilder.EnqueueChainBuild(req.Context(), layers, 0, "init", configID, forceRebuild, cleanBuild); err != nil {
			http.Error(w, fmt.Sprintf("failed to enqueue build: %s", err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"config_id": configID,
		"status":    "build_enqueued",
		"force":     fmt.Sprintf("%v", forceRebuild),
		"clean":     fmt.Sprintf("%v", cleanBuild),
	})
}

func (r *LayeredConfigRegistry) handleRefreshLayer(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse: /api/v1/layered-configs/{config_id}/layers/{layer_name}/refresh
	path := strings.TrimPrefix(req.URL.Path, "/api/v1/layered-configs/")
	parts := strings.SplitN(path, "/layers/", 2)
	if len(parts) != 2 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	configID := parts[0]
	layerName := strings.TrimSuffix(parts[1], "/refresh")

	// Load config and find the layer
	var cfgJSON string
	err := r.db.QueryRowContext(req.Context(), `SELECT config_json FROM layered_configs WHERE config_id = $1`, configID).Scan(&cfgJSON)
	if err != nil {
		http.Error(w, "config not found", http.StatusNotFound)
		return
	}

	var cfg snapshot.LayeredConfig
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		http.Error(w, "failed to parse stored config", http.StatusInternalServerError)
		return
	}

	layers := snapshot.MaterializeLayers(&cfg)
	startDepth := -1
	for i, l := range layers {
		if l.Name == layerName {
			startDepth = i
			break
		}
	}
	if startDepth < 0 {
		http.Error(w, fmt.Sprintf("layer %q not found in config", layerName), http.StatusNotFound)
		return
	}

	if r.layerBuilder != nil {
		if err := r.layerBuilder.EnqueueChainBuild(req.Context(), layers, startDepth, "refresh", configID); err != nil {
			http.Error(w, fmt.Sprintf("failed to enqueue refresh: %s", err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"config_id":  configID,
		"layer_name": layerName,
		"status":     "refresh_enqueued",
	})
}

// networkPolicyVal converts a json.RawMessage to a *string for DB storage.
// Returns nil (SQL NULL) when the policy is empty/null.
func networkPolicyVal(policy json.RawMessage) *string {
	if len(policy) == 0 || string(policy) == "null" {
		return nil
	}
	s := string(policy)
	return &s
}
