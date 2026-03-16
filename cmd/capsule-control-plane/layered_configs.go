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

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
	"github.com/rahul-roy-glean/capsule/pkg/tiers"
)

// LayeredConfigRegistry manages the layered_configs + snapshot_layers tables.
type LayeredConfigRegistry struct {
	db              *sql.DB
	snapshotManager *SnapshotManager
	layerBuilder    *LayerBuildScheduler
	configCache     *ConfigCache
	tagRegistry     *SnapshotTagRegistry
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

	// config_id = display_name (validated as a slug)
	if err := snapshot.ValidateConfigID(cfg.DisplayName); err != nil {
		return "", "", fmt.Errorf("invalid config_id: %w", err)
	}
	configID = cfg.DisplayName

	// Marshal config for storage
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal config: %w", err)
	}

	r.logger.WithFields(logrus.Fields{
		"config_id":         configID,
		"leaf_workload_key": leafWorkloadKey,
		"num_layers":        len(layers),
	}).Info("Registering layered config")

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	// Read current active workload_key for this config (might not exist for new configs)
	var oldLeafWK sql.NullString
	tx.QueryRowContext(ctx,
		`SELECT leaf_workload_key FROM config_workload_keys
		 WHERE config_id = $1 AND status = 'active'`,
		configID).Scan(&oldLeafWK)

	// Insert layers in topological order (root first).
	// The snapshot_layers UPSERT no longer overwrites config-scoped fields
	// (config_name, refresh_commands, refresh_interval, all_chain_drives) on
	// shared rows — those live in config_layer_settings instead.
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
				status = CASE WHEN snapshot_layers.status = 'inactive' THEN 'pending' ELSE snapshot_layers.status END,
				current_version = CASE WHEN snapshot_layers.status = 'inactive' THEN NULL ELSE snapshot_layers.current_version END,
				updated_at = NOW()
		`, layer.LayerHash, parentHash, layer.Name, layer.Depth,
			string(initCmdsJSON), string(refreshCmdsJSON), string(drivesJSON), string(allChainDrivesJSON), layer.RefreshInterval)
		if err != nil {
			return "", "", fmt.Errorf("failed to insert layer %s: %w", layer.Name, err)
		}

		// Upsert per-config layer settings (config-scoped fields)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO config_layer_settings (config_id, layer_hash, config_name,
				refresh_commands, refresh_interval, all_chain_drives)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (config_id, layer_hash) DO UPDATE SET
				config_name = EXCLUDED.config_name,
				refresh_commands = EXCLUDED.refresh_commands,
				refresh_interval = EXCLUDED.refresh_interval,
				all_chain_drives = EXCLUDED.all_chain_drives,
				updated_at = NOW()
		`, configID, layer.LayerHash, layer.Name,
			string(refreshCmdsJSON), layer.RefreshInterval, string(allChainDrivesJSON))
		if err != nil {
			return "", "", fmt.Errorf("failed to insert config_layer_settings for layer %s: %w", layer.Name, err)
		}
	}

	// If workload_key changed, drain old and activate new
	if oldLeafWK.Valid && oldLeafWK.String != "" && oldLeafWK.String != leafWorkloadKey {
		_, err = tx.ExecContext(ctx,
			`UPDATE config_workload_keys SET status = 'draining'
			 WHERE config_id = $1 AND status = 'active'`,
			configID)
		if err != nil {
			return "", "", fmt.Errorf("failed to drain old workload key: %w", err)
		}
	}

	// Upsert active workload_key
	_, err = tx.ExecContext(ctx,
		`INSERT INTO config_workload_keys (config_id, leaf_workload_key, leaf_layer_hash, status)
		 VALUES ($1, $2, $3, 'active')
		 ON CONFLICT (config_id, leaf_workload_key) DO UPDATE SET
		     leaf_layer_hash = EXCLUDED.leaf_layer_hash,
		     status = 'active'`,
		configID, leafWorkloadKey, leafLayer.LayerHash)
	if err != nil {
		return "", "", fmt.Errorf("failed to upsert config_workload_keys: %w", err)
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
			tier, start_command,
			runner_ttl_seconds, session_max_age_seconds, auto_pause, auto_rollout,
			max_concurrent_runners, build_schedule, network_policy_preset, network_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (config_id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			config_json = EXCLUDED.config_json,
			leaf_layer_hash = EXCLUDED.leaf_layer_hash,
			leaf_workload_key = EXCLUDED.leaf_workload_key,
			tier = EXCLUDED.tier,
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
		tierName, startCommandJSON,
		cfg.Config.TTL, cfg.Config.SessionMaxAgeSeconds, cfg.Config.AutoPause, cfg.Config.AutoRollout,
		0, "", cfg.Config.NetworkPolicyPreset, networkPolicyVal(cfg.Config.NetworkPolicy))

	if err != nil {
		return "", "", fmt.Errorf("failed to insert layered config: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", err
	}

	// Update in-memory workload config cache
	if r.configCache != nil {
		npJSON := ""
		if len(cfg.Config.NetworkPolicy) > 0 && string(cfg.Config.NetworkPolicy) != "null" {
			npJSON = string(cfg.Config.NetworkPolicy)
		}
		authJSON := ""
		if cfg.Config.Auth != nil {
			if ab, err := json.Marshal(cfg.Config.Auth); err == nil {
				authJSON = string(ab)
			}
		}
		r.configCache.PutWorkloadConfig(&WorkloadConfig{
			WorkloadKey:          leafWorkloadKey,
			Tier:                 tierName,
			StartCommand:         cfg.StartCommand,
			RunnerTTLSeconds:     cfg.Config.TTL,
			SessionMaxAgeSeconds: cfg.Config.SessionMaxAgeSeconds,
			AutoPause:            cfg.Config.AutoPause,
			MaxConcurrentRunners: 0,
			NetworkPolicyPreset:  cfg.Config.NetworkPolicyPreset,
			NetworkPolicyJSON:    npJSON,
			AuthConfigJSON:       authJSON,
		})
	}
	return configID, leafWorkloadKey, nil
}

// GetLayeredConfig returns a stored layered config by config_id.
func (r *LayeredConfigRegistry) GetLayeredConfig(ctx context.Context, configID string) (*StoredLayeredConfig, error) {
	var sc StoredLayeredConfig
	var startCommandJSON sql.NullString
	var npPreset sql.NullString
	var npJSON sql.NullString

	err := r.db.QueryRowContext(ctx, `
		SELECT config_id, display_name, leaf_layer_hash, leaf_workload_key,
		       tier, start_command,
		       runner_ttl_seconds, session_max_age_seconds, auto_pause, auto_rollout,
		       max_concurrent_runners, build_schedule, network_policy_preset, network_policy,
		       created_at, updated_at
		FROM layered_configs WHERE config_id = $1
	`, configID).Scan(&sc.ConfigID, &sc.DisplayName, &sc.LeafLayerHash, &sc.LeafWorkloadKey,
		&sc.Tier, &startCommandJSON,
		&sc.RunnerTTLSeconds, &sc.SessionMaxAgeSeconds, &sc.AutoPause, &sc.AutoRollout,
		&sc.MaxConcurrentRunners, &sc.BuildSchedule, &npPreset, &npJSON,
		&sc.CreatedAt, &sc.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("layered config not found: %s", configID)
	}
	if err != nil {
		return nil, err
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
		       tier, start_command,
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
		var startCommandJSON sql.NullString

		if err := rows.Scan(&sc.ConfigID, &sc.DisplayName, &sc.LeafLayerHash, &sc.LeafWorkloadKey,
			&sc.Tier, &startCommandJSON,
			&sc.RunnerTTLSeconds, &sc.SessionMaxAgeSeconds, &sc.AutoPause, &sc.AutoRollout,
			&sc.MaxConcurrentRunners, &sc.BuildSchedule, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
			return nil, err
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
			SELECT layer_hash, parent_layer_hash, depth, status, current_version
			FROM snapshot_layers WHERE layer_hash = $1
			UNION ALL
			SELECT sl.layer_hash, sl.parent_layer_hash, sl.depth, sl.status, sl.current_version
			FROM snapshot_layers sl
			JOIN chain c ON sl.layer_hash = c.parent_layer_hash
		)
		SELECT c.layer_hash,
		       cls.config_name,
		       c.depth, c.status, c.current_version,
		       sb.status, sb.version
		FROM chain c
		JOIN config_layer_settings cls ON cls.layer_hash = c.layer_hash AND cls.config_id = $2
		LEFT JOIN snapshot_builds sb ON sb.layer_hash = c.layer_hash
			AND sb.status IN ('queued', 'waiting_parent', 'running')
		ORDER BY c.depth
	`, leafHash, configID)
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
// All changes are wrapped in a transaction for atomicity [L2].
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

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Collect all workload_keys for this config (active + draining) before deleting
	var allWKs []string
	wkRows, err := tx.QueryContext(ctx,
		`SELECT leaf_workload_key FROM config_workload_keys WHERE config_id = $1`, configID)
	if err == nil {
		for wkRows.Next() {
			var wk string
			wkRows.Scan(&wk)
			allWKs = append(allWKs, wk)
		}
		wkRows.Close()
	}

	// Delete config_workload_keys, config_layer_settings, and the config
	tx.ExecContext(ctx, `DELETE FROM config_workload_keys WHERE config_id = $1`, configID)
	tx.ExecContext(ctx, `DELETE FROM config_layer_settings WHERE config_id = $1`, configID)

	// Delete the config
	result, err := tx.ExecContext(ctx, `DELETE FROM layered_configs WHERE config_id = $1`, configID)
	if err != nil {
		return err
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("layered config not found: %s", configID)
	}

	// For each layer, check if it's still referenced by another config.
	// Walk the parent chain from each remaining config's leaf_layer_hash.
	// If the layer is in any config's chain, preserve it.
	for _, layer := range layers {
		var referenced int
		tx.QueryRowContext(ctx, `
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
		tx.ExecContext(ctx, `UPDATE snapshot_layers SET status='inactive', current_version=NULL WHERE layer_hash=$1`, layer.LayerHash)

		// Cancel active builds [M4]
		tx.ExecContext(ctx, `
			UPDATE snapshot_builds SET status='cancelled'
			WHERE layer_hash=$1 AND status IN ('queued','waiting_parent','running')
		`, layer.LayerHash)

		r.logger.WithFields(logrus.Fields{
			"layer_hash": layer.LayerHash[:16],
			"layer_name": layer.Name,
		}).Info("Deactivated orphaned layer and cancelled builds")
	}

	// Clean up workload_key metadata for all workload_keys (active + draining)
	// that are no longer referenced by any other config
	for _, wk := range allWKs {
		var otherCount int
		tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM config_workload_keys WHERE leaf_workload_key = $1`,
			wk).Scan(&otherCount)
		if otherCount == 0 {
			tx.ExecContext(ctx, `DELETE FROM version_assignments WHERE workload_key = $1`, wk)
			tx.ExecContext(ctx, `UPDATE snapshots SET status = 'deprecated' WHERE workload_key = $1 AND status = 'active'`, wk)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Invalidate the workload config cache so the next allocation re-reads from DB.
	// If another config shares the same leaf_workload_key, the cache will be
	// repopulated on the next cache miss.
	if r.configCache != nil && leafWorkloadKey != "" {
		r.configCache.InvalidateWorkloadConfig(leafWorkloadKey)
	}

	return nil
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
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
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

	// Route /{wk}/tags, /{wk}/tags/{tag}, /{wk}/promote to tag registry
	parts := strings.SplitN(path, "/", 3)
	if len(parts) >= 2 && r.tagRegistry != nil {
		wk := parts[0]
		sub := parts[1]
		if sub == "tags" {
			subpath := ""
			if len(parts) == 3 {
				subpath = parts[2]
			}
			r.tagRegistry.HandleTags(w, req, wk, subpath)
			return
		}
		if sub == "promote" {
			r.handlePromote(w, req, wk)
			return
		}
	}

	switch req.Method {
	case http.MethodGet:
		r.handleGetLayeredConfig(w, req)
	case http.MethodDelete:
		r.handleDeleteLayeredConfig(w, req)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handlePromote promotes a tagged version to active, updating snapshot status
// and version assignments so hosts converge to the new version.
func (r *LayeredConfigRegistry) handlePromote(w http.ResponseWriter, req *http.Request, workloadKey string) {
	if req.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Tag == "" {
		writeAPIError(w, http.StatusBadRequest, "tag is required")
		return
	}
	version, err := r.tagRegistry.PromoteTag(req.Context(), workloadKey, body.Tag)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeAPIError(w, http.StatusNotFound, err.Error())
		} else {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// Activate the snapshot and assign the version fleet-wide so hosts converge.
	if r.snapshotManager != nil {
		if err := r.snapshotManager.SetActiveSnapshotForKey(req.Context(), workloadKey, version); err != nil {
			r.logger.WithError(err).Warn("promote: failed to set active snapshot")
		}
		if err := r.snapshotManager.AssignVersion(req.Context(), workloadKey, nil, version); err != nil {
			r.logger.WithError(err).Warn("promote: failed to assign version")
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"workload_key": workloadKey,
		"tag":          body.Tag,
		"version":      version,
		"status":       "promoted",
	})
}

func (r *LayeredConfigRegistry) handleCreateLayeredConfig(w http.ResponseWriter, req *http.Request) {
	var cfg snapshot.LayeredConfig
	if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	configID, leafWorkloadKey, err := r.RegisterLayeredConfig(req.Context(), &cfg)
	if err != nil {
		if strings.Contains(err.Error(), "invalid") {
			writeAPIError(w, http.StatusBadRequest, err.Error())
		} else {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// Get actual layer statuses from DB (layers may already be active if shared)
	layers := snapshot.MaterializeLayers(&cfg)
	layerInfos := make([]map[string]interface{}, len(layers))
	for i, l := range layers {
		layerStatus := "pending"
		var currentVersion sql.NullString
		var dbStatus sql.NullString
		r.db.QueryRowContext(req.Context(),
			`SELECT status, current_version FROM snapshot_layers WHERE layer_hash = $1`,
			l.LayerHash).Scan(&dbStatus, &currentVersion)
		if dbStatus.Valid {
			layerStatus = dbStatus.String
		}
		info := map[string]interface{}{
			"name":   l.Name,
			"hash":   l.LayerHash,
			"depth":  l.Depth,
			"status": layerStatus,
		}
		if currentVersion.Valid && currentVersion.String != "" {
			info["current_version"] = currentVersion.String
		}
		layerInfos[i] = info
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
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	configs = nonNilSlice(configs)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"configs": configs, "count": len(configs)})
}

func (r *LayeredConfigRegistry) handleGetLayeredConfig(w http.ResponseWriter, req *http.Request) {
	configID := strings.TrimPrefix(req.URL.Path, "/api/v1/layered-configs/")
	if configID == "" {
		writeAPIError(w, http.StatusBadRequest, "config_id is required")
		return
	}

	sc, err := r.GetLayeredConfig(req.Context(), configID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeAPIError(w, http.StatusNotFound, err.Error())
		} else {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
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
		writeAPIError(w, http.StatusBadRequest, "config_id is required")
		return
	}

	if err := r.DeleteLayeredConfig(req.Context(), configID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeAPIError(w, http.StatusNotFound, err.Error())
		} else {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (r *LayeredConfigRegistry) handleTriggerBuild(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	path := strings.TrimPrefix(req.URL.Path, "/api/v1/layered-configs/")
	configID := strings.TrimSuffix(path, "/build")
	if configID == "" {
		writeAPIError(w, http.StatusBadRequest, "config_id is required")
		return
	}

	// Load config and materialize layers
	var cfgJSON string
	err := r.db.QueryRowContext(req.Context(), `SELECT config_json FROM layered_configs WHERE config_id = $1`, configID).Scan(&cfgJSON)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "config not found")
		return
	}

	var cfg snapshot.LayeredConfig
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "failed to parse stored config")
		return
	}

	layers := snapshot.MaterializeLayers(&cfg)
	forceRebuild := req.URL.Query().Get("force") == "true"
	cleanBuild := req.URL.Query().Get("clean") == "true"
	if cleanBuild {
		forceRebuild = true
	}

	var enqueued int
	if r.layerBuilder != nil {
		var err error
		enqueued, err = r.layerBuilder.EnqueueChainBuild(req.Context(), layers, 0, "init", configID, forceRebuild, cleanBuild)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to enqueue build: %s", err))
			return
		}
	}

	status := "build_enqueued"
	httpStatus := http.StatusAccepted
	if enqueued == 0 {
		status = "no_build_needed"
		httpStatus = http.StatusOK
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"config_id": configID,
		"status":    status,
		"enqueued":  enqueued,
		"force":     forceRebuild,
		"clean":     cleanBuild,
	})
}

func (r *LayeredConfigRegistry) handleRefreshLayer(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Parse: /api/v1/layered-configs/{config_id}/layers/{layer_name}/refresh
	path := strings.TrimPrefix(req.URL.Path, "/api/v1/layered-configs/")
	parts := strings.SplitN(path, "/layers/", 2)
	if len(parts) != 2 {
		writeAPIError(w, http.StatusBadRequest, "invalid path")
		return
	}
	configID := parts[0]
	layerName := strings.TrimSuffix(parts[1], "/refresh")

	// Load config and find the layer
	var cfgJSON string
	err := r.db.QueryRowContext(req.Context(), `SELECT config_json FROM layered_configs WHERE config_id = $1`, configID).Scan(&cfgJSON)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "config not found")
		return
	}

	var cfg snapshot.LayeredConfig
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "failed to parse stored config")
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
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("layer %q not found in config", layerName))
		return
	}

	var enqueued int
	if r.layerBuilder != nil {
		var err error
		enqueued, err = r.layerBuilder.EnqueueChainBuild(req.Context(), layers, startDepth, "refresh", configID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to enqueue refresh: %s", err))
			return
		}
	}

	status := "refresh_enqueued"
	httpStatus := http.StatusAccepted
	if enqueued == 0 {
		status = "no_refresh_needed"
		httpStatus = http.StatusOK
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"config_id":  configID,
		"layer_name": layerName,
		"status":     status,
		"enqueued":   enqueued,
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
