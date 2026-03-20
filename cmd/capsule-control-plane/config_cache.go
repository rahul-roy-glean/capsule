package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

// WorkloadConfig holds the frequently-accessed config fields for a workload_key.
// These are read on every runner allocation and rarely change.
type WorkloadConfig struct {
	WorkloadKey                  string
	Tier                         string
	StartCommand                 *snapshot.StartCommand
	RunnerTTLSeconds             int
	SessionMaxAgeSeconds         int
	CheckpointIntervalSeconds    int
	CheckpointQuietWindowSeconds int
	AutoPause                    bool
	MaxConcurrentRunners         int
	NetworkPolicyPreset          string
	NetworkPolicyJSON            string
	AuthConfigJSON               string // JSON-encoded authproxy.AuthConfig for injection into host agent
}

// ConfigCache provides in-memory lookup for workload_key→config metadata.
// Loaded from DB on startup, updated on registration, with DB fallback on cache miss.
type ConfigCache struct {
	mu             sync.RWMutex
	workloadConfig map[string]*WorkloadConfig // workload_key → config
	db             *sql.DB
	logger         *logrus.Entry
}

// NewConfigCache creates and loads a ConfigCache from the database.
func NewConfigCache(db *sql.DB, logger *logrus.Logger) *ConfigCache {
	cc := &ConfigCache{
		workloadConfig: make(map[string]*WorkloadConfig),
		db:             db,
		logger:         logger.WithField("component", "config-cache"),
	}
	cc.loadFromDB()
	return cc
}

// loadFromDB populates the cache from all config tables.
func (cc *ConfigCache) loadFromDB() {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	lcRows, err := cc.db.Query(`SELECT DISTINCT ON (leaf_workload_key) leaf_workload_key, tier, start_command, runner_ttl_seconds, session_max_age_seconds, checkpoint_interval_seconds, checkpoint_quiet_window_seconds, auto_pause, max_concurrent_runners, network_policy_preset, network_policy, config_json FROM layered_configs ORDER BY leaf_workload_key, created_at DESC`)
	if err == nil {
		defer lcRows.Close()
		for lcRows.Next() {
			var wc WorkloadConfig
			var startCmdJSON sql.NullString
			var npPreset sql.NullString
			var npJSON sql.NullString
			var configJSON sql.NullString
			if err := lcRows.Scan(&wc.WorkloadKey, &wc.Tier, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.CheckpointIntervalSeconds, &wc.CheckpointQuietWindowSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON, &configJSON); err != nil {
				continue
			}
			if startCmdJSON.Valid && startCmdJSON.String != "" {
				wc.StartCommand = &snapshot.StartCommand{}
				json.Unmarshal([]byte(startCmdJSON.String), wc.StartCommand)
			}
			if npPreset.Valid {
				wc.NetworkPolicyPreset = npPreset.String
			}
			if npJSON.Valid {
				wc.NetworkPolicyJSON = npJSON.String
			}
			if configJSON.Valid {
				wc.AuthConfigJSON = extractAuthConfigJSON(configJSON.String)
			}
			cc.workloadConfig[wc.WorkloadKey] = &wc
		}
	}

	// Also load draining/old workload keys from config_workload_keys so that
	// allocations using a previous workload_key still resolve their config.
	cwkRows, err := cc.db.Query(`
		SELECT cwk.leaf_workload_key, lc.tier, lc.start_command, lc.runner_ttl_seconds,
		       lc.session_max_age_seconds, lc.checkpoint_interval_seconds, lc.checkpoint_quiet_window_seconds, lc.auto_pause, lc.max_concurrent_runners,
		       lc.network_policy_preset, lc.network_policy, lc.config_json
		FROM config_workload_keys cwk
		JOIN layered_configs lc ON lc.config_id = cwk.config_id
		WHERE cwk.leaf_workload_key NOT IN (SELECT leaf_workload_key FROM layered_configs)`)
	if err == nil {
		defer cwkRows.Close()
		for cwkRows.Next() {
			var wc WorkloadConfig
			var startCmdJSON sql.NullString
			var npPreset sql.NullString
			var npJSON sql.NullString
			var configJSON sql.NullString
			if err := cwkRows.Scan(&wc.WorkloadKey, &wc.Tier, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.CheckpointIntervalSeconds, &wc.CheckpointQuietWindowSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON, &configJSON); err != nil {
				continue
			}
			if startCmdJSON.Valid && startCmdJSON.String != "" {
				wc.StartCommand = &snapshot.StartCommand{}
				json.Unmarshal([]byte(startCmdJSON.String), wc.StartCommand)
			}
			if npPreset.Valid {
				wc.NetworkPolicyPreset = npPreset.String
			}
			if npJSON.Valid {
				wc.NetworkPolicyJSON = npJSON.String
			}
			if configJSON.Valid {
				wc.AuthConfigJSON = extractAuthConfigJSON(configJSON.String)
			}
			// Only add if not already loaded from layered_configs (active takes precedence)
			if _, exists := cc.workloadConfig[wc.WorkloadKey]; !exists {
				cc.workloadConfig[wc.WorkloadKey] = &wc
			}
		}
	}

	cc.logger.WithFields(logrus.Fields{
		"workload_configs": len(cc.workloadConfig),
	}).Info("Config cache loaded from DB")
}

// GetWorkloadConfig returns the config for a workload_key, or nil if not found.
// Checks in-memory cache first, then falls back to DB and backfills.
func (cc *ConfigCache) GetWorkloadConfig(ctx context.Context, workloadKey string) *WorkloadConfig {
	cc.mu.RLock()
	wc, ok := cc.workloadConfig[workloadKey]
	cc.mu.RUnlock()
	if ok {
		return wc
	}

	// Cache miss: try DB
	wc = cc.loadWorkloadConfigFromDB(ctx, workloadKey)
	if wc != nil {
		cc.mu.Lock()
		cc.workloadConfig[workloadKey] = wc
		cc.mu.Unlock()
	}
	return wc
}

// loadWorkloadConfigFromDB loads a single workload config from the DB.
func (cc *ConfigCache) loadWorkloadConfigFromDB(ctx context.Context, workloadKey string) *WorkloadConfig {
	var wc WorkloadConfig
	var startCmdJSON sql.NullString
	var npPreset sql.NullString
	var npJSON sql.NullString
	wc.WorkloadKey = workloadKey

	// Try layered_configs by direct leaf_workload_key match first
	var configJSON sql.NullString
	err := cc.db.QueryRowContext(ctx,
		`SELECT tier, start_command, runner_ttl_seconds, session_max_age_seconds, checkpoint_interval_seconds, checkpoint_quiet_window_seconds, auto_pause, max_concurrent_runners, network_policy_preset, network_policy, config_json
		 FROM layered_configs WHERE leaf_workload_key = $1 ORDER BY created_at DESC LIMIT 1`, workloadKey).Scan(
		&wc.Tier, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.CheckpointIntervalSeconds, &wc.CheckpointQuietWindowSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON, &configJSON)
	if err != nil {
		// Fallback: the workload_key may be from a previous config version (draining).
		// Look up the config_id via config_workload_keys, then load the current config.
		err = cc.db.QueryRowContext(ctx,
			`SELECT lc.tier, lc.start_command, lc.runner_ttl_seconds, lc.session_max_age_seconds, lc.checkpoint_interval_seconds, lc.checkpoint_quiet_window_seconds, lc.auto_pause, lc.max_concurrent_runners, lc.network_policy_preset, lc.network_policy, lc.config_json
			 FROM config_workload_keys cwk
			 JOIN layered_configs lc ON lc.config_id = cwk.config_id
			 WHERE cwk.leaf_workload_key = $1
			 ORDER BY lc.created_at DESC LIMIT 1`, workloadKey).Scan(
			&wc.Tier, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.CheckpointIntervalSeconds, &wc.CheckpointQuietWindowSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON, &configJSON)
		if err != nil {
			return nil
		}
	}
	if startCmdJSON.Valid && startCmdJSON.String != "" {
		wc.StartCommand = &snapshot.StartCommand{}
		json.Unmarshal([]byte(startCmdJSON.String), wc.StartCommand)
	}
	if npPreset.Valid {
		wc.NetworkPolicyPreset = npPreset.String
	}
	if npJSON.Valid {
		wc.NetworkPolicyJSON = npJSON.String
	}
	if configJSON.Valid {
		wc.AuthConfigJSON = extractAuthConfigJSON(configJSON.String)
	}
	return &wc
}

// PutWorkloadConfig adds or updates a workload config in the cache.
func (cc *ConfigCache) PutWorkloadConfig(wc *WorkloadConfig) {
	cc.mu.Lock()
	cc.workloadConfig[wc.WorkloadKey] = wc
	cc.mu.Unlock()
}

// extractAuthConfigJSON extracts the "config.auth" field from a LayeredConfig JSON blob.
func extractAuthConfigJSON(configJSON string) string {
	var raw struct {
		Config struct {
			Auth json.RawMessage `json:"auth"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil || len(raw.Config.Auth) == 0 || string(raw.Config.Auth) == "null" {
		return ""
	}
	return string(raw.Config.Auth)
}

// InvalidateWorkloadConfig removes a workload config from the cache.
func (cc *ConfigCache) InvalidateWorkloadConfig(workloadKey string) {
	cc.mu.Lock()
	delete(cc.workloadConfig, workloadKey)
	cc.mu.Unlock()
}
