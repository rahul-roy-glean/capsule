package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// WorkloadConfig holds the frequently-accessed config fields for a workload_key.
// These are read on every runner allocation and rarely change.
type WorkloadConfig struct {
	WorkloadKey          string
	Tier                 string
	CISystem             string
	StartCommand         *snapshot.StartCommand
	RunnerTTLSeconds     int
	SessionMaxAgeSeconds int
	AutoPause            bool
	MaxConcurrentRunners int
	NetworkPolicyPreset  string
	NetworkPolicyJSON    string
}

// ConfigCache provides in-memory lookup for repo→workload_key mappings and
// workload_key→config metadata. Loaded from DB on startup, updated on
// registration, with DB fallback on cache miss.
type ConfigCache struct {
	mu             sync.RWMutex
	repoToWorkload map[string]string          // repo (owner/name) → workload_key
	workloadConfig map[string]*WorkloadConfig // workload_key → config
	db             *sql.DB
	logger         *logrus.Entry
}

// NewConfigCache creates and loads a ConfigCache from the database.
func NewConfigCache(db *sql.DB, logger *logrus.Logger) *ConfigCache {
	cc := &ConfigCache{
		repoToWorkload: make(map[string]string),
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

	// Load repo_workload_mappings
	rows, err := cc.db.Query(`SELECT repo, workload_key FROM repo_workload_mappings`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var repo, wk string
			if err := rows.Scan(&repo, &wk); err == nil {
				cc.repoToWorkload[repo] = wk
			}
		}
	}

	// Load layered_configs
	lcRows, err := cc.db.Query(`SELECT leaf_workload_key, tier, ci_system, start_command, runner_ttl_seconds, session_max_age_seconds, auto_pause, max_concurrent_runners, network_policy_preset, network_policy FROM layered_configs`)
	if err == nil {
		defer lcRows.Close()
		for lcRows.Next() {
			var wc WorkloadConfig
			var startCmdJSON sql.NullString
			var npPreset sql.NullString
			var npJSON sql.NullString
			if err := lcRows.Scan(&wc.WorkloadKey, &wc.Tier, &wc.CISystem, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON); err != nil {
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
			cc.workloadConfig[wc.WorkloadKey] = &wc
		}
	}

	cc.logger.WithFields(logrus.Fields{
		"repo_mappings":    len(cc.repoToWorkload),
		"workload_configs": len(cc.workloadConfig),
	}).Info("Config cache loaded from DB")
}

// GetWorkloadKeyForRepo returns the workload_key for a repo, or "" if not found.
// Checks in-memory cache first, then falls back to DB and backfills.
func (cc *ConfigCache) GetWorkloadKeyForRepo(repo string) string {
	cc.mu.RLock()
	wk, ok := cc.repoToWorkload[repo]
	cc.mu.RUnlock()
	if ok {
		return wk
	}

	// Cache miss: fall back to DB
	wk = lookupWorkloadKeyForRepo(cc.db, repo)
	if wk != "" {
		cc.mu.Lock()
		cc.repoToWorkload[repo] = wk
		cc.mu.Unlock()
	}
	return wk
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

	// Try layered_configs first
	err := cc.db.QueryRowContext(ctx,
		`SELECT tier, ci_system, start_command, runner_ttl_seconds, session_max_age_seconds, auto_pause, max_concurrent_runners, network_policy_preset, network_policy
		 FROM layered_configs WHERE leaf_workload_key = $1`, workloadKey).Scan(
		&wc.Tier, &wc.CISystem, &startCmdJSON, &wc.RunnerTTLSeconds, &wc.SessionMaxAgeSeconds, &wc.AutoPause, &wc.MaxConcurrentRunners, &npPreset, &npJSON)
	if err != nil {
		return nil
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
	return &wc
}

// PutRepoMapping adds or updates a repo→workload_key mapping in the cache.
func (cc *ConfigCache) PutRepoMapping(repo, workloadKey string) {
	cc.mu.Lock()
	cc.repoToWorkload[repo] = workloadKey
	cc.mu.Unlock()
}

// PutWorkloadConfig adds or updates a workload config in the cache.
func (cc *ConfigCache) PutWorkloadConfig(wc *WorkloadConfig) {
	cc.mu.Lock()
	cc.workloadConfig[wc.WorkloadKey] = wc
	cc.mu.Unlock()
}

// InvalidateWorkloadConfig removes a workload config from the cache.
func (cc *ConfigCache) InvalidateWorkloadConfig(workloadKey string) {
	cc.mu.Lock()
	delete(cc.workloadConfig, workloadKey)
	cc.mu.Unlock()
}
