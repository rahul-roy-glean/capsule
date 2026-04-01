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
	WorkloadKey          string
	Tier                 string
	StartCommand         *snapshot.StartCommand
	RunnerTTLSeconds     int
	SessionMaxAgeSeconds int
	AutoPause            bool
	MaxConcurrentRunners int
	NetworkPolicyPreset  string
	NetworkPolicyJSON    string

	// Access plane fields — loaded from project_access_planes table
	// based on the project that owns this workload.
	AccessPlaneAddr      string // HTTP API address (e.g. "http://access-plane:8080")
	AccessPlaneProxyAddr string // CONNECT proxy address (e.g. "access-plane:3128")
	AttestationSecret    string // HMAC secret for minting runner tokens
	CACertPEM            string // CA cert for SSL bump
	TenantID             string // Tenant identifier for the access plane
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

	lcRows, err := cc.db.Query(`SELECT DISTINCT ON (leaf_workload_key) leaf_workload_key, tier, start_command, runner_ttl_seconds, session_max_age_seconds, auto_pause, max_concurrent_runners, network_policy_preset, network_policy, config_json FROM layered_configs ORDER BY leaf_workload_key, created_at DESC`)
	if err == nil {
		defer lcRows.Close()
		for lcRows.Next() {
			var wc WorkloadConfig
			var startCmdJSON sql.NullString
			var npPreset sql.NullString
			var npJSON sql.NullString
			var configJSON sql.NullString
			if err := lcRows.Scan(&wc.WorkloadKey, &wc.Tier, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON, &configJSON); err != nil {
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
				// Access plane config is loaded separately from project_access_planes table.
			}
			cc.workloadConfig[wc.WorkloadKey] = &wc
		}
	}

	// Also load draining/old workload keys from config_workload_keys so that
	// allocations using a previous workload_key still resolve their config.
	cwkRows, err := cc.db.Query(`
		SELECT cwk.leaf_workload_key, lc.tier, lc.start_command, lc.runner_ttl_seconds,
		       lc.session_max_age_seconds, lc.auto_pause, lc.max_concurrent_runners,
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
			if err := cwkRows.Scan(&wc.WorkloadKey, &wc.Tier, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON, &configJSON); err != nil {
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
				// Access plane config is loaded separately from project_access_planes table.
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
		`SELECT tier, start_command, runner_ttl_seconds, session_max_age_seconds, auto_pause, max_concurrent_runners, network_policy_preset, network_policy, config_json
		 FROM layered_configs WHERE leaf_workload_key = $1 ORDER BY created_at DESC LIMIT 1`, workloadKey).Scan(
		&wc.Tier, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON, &configJSON)
	if err != nil {
		// Fallback: the workload_key may be from a previous config version (draining).
		// Look up the config_id via config_workload_keys, then load the current config.
		err = cc.db.QueryRowContext(ctx,
			`SELECT lc.tier, lc.start_command, lc.runner_ttl_seconds, lc.session_max_age_seconds, lc.auto_pause, lc.max_concurrent_runners, lc.network_policy_preset, lc.network_policy, lc.config_json
			 FROM config_workload_keys cwk
			 JOIN layered_configs lc ON lc.config_id = cwk.config_id
			 WHERE cwk.leaf_workload_key = $1
			 ORDER BY lc.created_at DESC LIMIT 1`, workloadKey).Scan(
			&wc.Tier, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON, &configJSON)
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
		// Access plane config is loaded separately from project_access_planes table.
	}
	return &wc
}

// PutWorkloadConfig adds or updates a workload config in the cache.
func (cc *ConfigCache) PutWorkloadConfig(wc *WorkloadConfig) {
	cc.mu.Lock()
	cc.workloadConfig[wc.WorkloadKey] = wc
	cc.mu.Unlock()
}

// LoadAccessPlaneForProject looks up the access plane configuration for a
// project from the project_access_planes table. Returns nil if no access plane
// is configured for the project.
func (cc *ConfigCache) LoadAccessPlaneForProject(ctx context.Context, projectID string) *AccessPlaneInfo {
	if projectID == "" {
		return nil
	}
	var info AccessPlaneInfo
	err := cc.db.QueryRowContext(ctx,
		`SELECT access_plane_addr, proxy_addr, attestation_secret_ref, COALESCE(ca_cert_pem, ''), tenant_id
		 FROM project_access_planes WHERE project_id = $1`, projectID).Scan(
		&info.Addr, &info.ProxyAddr, &info.AttestationSecretRef, &info.CACertPEM, &info.TenantID)
	if err != nil {
		return nil
	}
	info.ProjectID = projectID
	return &info
}

// AccessPlaneInfo holds the access plane deployment details for a project.
type AccessPlaneInfo struct {
	ProjectID            string
	Addr                 string // HTTP API address
	ProxyAddr            string // CONNECT proxy address
	AttestationSecretRef string // sm:project/secret reference
	CACertPEM            string
	TenantID             string
}

// InvalidateWorkloadConfig removes a workload config from the cache.
func (cc *ConfigCache) InvalidateWorkloadConfig(workloadKey string) {
	cc.mu.Lock()
	delete(cc.workloadConfig, workloadKey)
	cc.mu.Unlock()
}
