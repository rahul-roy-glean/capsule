package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/api/compute/v1"
)

type downscalerConfig struct {
	Enabled              bool
	ProjectID            string
	Region               string
	MIGName              string
	Interval             time.Duration
	MaxDeletesPerCycle   int
	MaxDrainsPerCycle    int
	HeartbeatStaleWindow time.Duration
	ScaleUpThreshold     float64
	ScaleDownThreshold   float64
	Cooldown             time.Duration
}

func loadDownscalerConfig(logger *logrus.Entry) downscalerConfig {
	projectID := strings.TrimSpace(os.Getenv("GCP_PROJECT_ID"))
	region := strings.TrimSpace(os.Getenv("GCP_REGION"))
	migName := strings.TrimSpace(os.Getenv("HOST_MIG_NAME"))

	enabled := false
	if raw := strings.TrimSpace(os.Getenv("DOWNSCALER_ENABLED")); raw != "" {
		if v, err := strconv.ParseBool(raw); err == nil {
			enabled = v
		}
	} else {
		enabled = projectID != "" && region != "" && migName != ""
	}

	interval := 60 * time.Second
	if raw := strings.TrimSpace(os.Getenv("DOWNSCALER_INTERVAL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			interval = d
		} else {
			logger.WithField("value", raw).Warn("Invalid DOWNSCALER_INTERVAL; using default")
		}
	}

	maxDeletes := 1
	if raw := strings.TrimSpace(os.Getenv("DOWNSCALER_MAX_DELETES")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			maxDeletes = v
		}
	}
	maxDrains := maxDeletes
	if raw := strings.TrimSpace(os.Getenv("DOWNSCALER_MAX_DRAINS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			maxDrains = v
		}
	}

	stale := 90 * time.Second
	if raw := strings.TrimSpace(os.Getenv("DOWNSCALER_HEARTBEAT_STALE")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			stale = d
		}
	}

	scaleUpThreshold := 0.9
	if raw := strings.TrimSpace(os.Getenv("AUTOSCALER_SCALE_UP_THRESHOLD")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 && v <= 1 {
			scaleUpThreshold = v
		}
	}

	scaleDownThreshold := 0.5
	if raw := strings.TrimSpace(os.Getenv("AUTOSCALER_SCALE_DOWN_THRESHOLD")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 && v <= 1 {
			scaleDownThreshold = v
		}
	}

	cooldown := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("AUTOSCALER_COOLDOWN")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			cooldown = d
		}
	}

	return downscalerConfig{
		Enabled:              enabled,
		ProjectID:            projectID,
		Region:               region,
		MIGName:              migName,
		Interval:             interval,
		MaxDeletesPerCycle:   maxDeletes,
		MaxDrainsPerCycle:    maxDrains,
		HeartbeatStaleWindow: stale,
		ScaleUpThreshold:     scaleUpThreshold,
		ScaleDownThreshold:   scaleDownThreshold,
		Cooldown:             cooldown,
	}
}

func startDownscaler(ctx context.Context, db *sql.DB, hr *HostRegistry, sched *Scheduler, logger *logrus.Logger) {
	log := logger.WithField("component", "downscaler")
	cfg := loadDownscalerConfig(log)
	if !cfg.Enabled {
		log.Info("Downscaler disabled")
		return
	}

	svc, err := compute.NewService(ctx)
	if err != nil {
		log.WithError(err).Warn("Failed to create GCP compute client; downscaler disabled")
		return
	}

	log.WithFields(logrus.Fields{
		"project":              cfg.ProjectID,
		"region":               cfg.Region,
		"mig":                  cfg.MIGName,
		"interval":             cfg.Interval.String(),
		"max_deletes":          cfg.MaxDeletesPerCycle,
		"max_drains":           cfg.MaxDrainsPerCycle,
		"stale_window_s":       int(cfg.HeartbeatStaleWindow.Seconds()),
		"scale_up_threshold":   cfg.ScaleUpThreshold,
		"scale_down_threshold": cfg.ScaleDownThreshold,
		"cooldown":             cfg.Cooldown.String(),
	}).Info("Starting downscaler")

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	var lastScaleAction time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := tryAdvisoryLock(ctx, db, 42424242)
			if err != nil {
				log.WithError(err).Warn("Failed to acquire downscaler lock")
				continue
			}
			if !ok {
				continue
			}

			if acted, err := runDownscaleOnce(ctx, cfg, svc, hr, sched, lastScaleAction, log); err != nil {
				log.WithError(err).Warn("Downscale iteration failed")
			} else if acted {
				lastScaleAction = time.Now()
			}

			_ = advisoryUnlock(ctx, db, 42424242)
		}
	}
}

// runDownscaleOnce returns (actionTaken, error). actionTaken is true when a
// scale-up resize or scale-down drain was performed (used for cooldown tracking).
func runDownscaleOnce(ctx context.Context, cfg downscalerConfig, svc *compute.Service, hr *HostRegistry, sched *Scheduler, lastScaleAction time.Time, log *logrus.Entry) (bool, error) {
	mig, err := svc.RegionInstanceGroupManagers.Get(cfg.ProjectID, cfg.Region, cfg.MIGName).Context(ctx).Do()
	if err != nil {
		return false, fmt.Errorf("get mig: %w", err)
	}
	currentTarget := mig.TargetSize

	managed, err := svc.RegionInstanceGroupManagers.ListManagedInstances(cfg.ProjectID, cfg.Region, cfg.MIGName).Context(ctx).Do()
	if err != nil {
		return false, fmt.Errorf("list managed instances: %w", err)
	}

	instanceByName := map[string]string{}
	for _, mi := range managed.ManagedInstances {
		name := instanceNameFromURL(mi.Instance)
		if name == "" {
			continue
		}
		instanceByName[name] = mi.Instance
	}

	// Snapshot hosts map by instance name.
	hosts := hr.GetAllHosts()
	hostByName := map[string]*Host{}
	for _, h := range hosts {
		hostByName[h.InstanceName] = h
	}

	// Phase 0: clean up unhealthy and stale terminating hosts.
	unhealthyGracePeriod := 5 * cfg.HeartbeatStaleWindow
	phase0Deleted := 0
	for _, h := range hosts {
		if phase0Deleted >= cfg.MaxDeletesPerCycle {
			break
		}

		if h.Status == "unhealthy" && time.Since(h.LastHeartbeat) >= unhealthyGracePeriod {
			// Delete GCE instance if it exists in the MIG.
			if instanceURL, ok := instanceByName[h.InstanceName]; ok {
				req := &compute.RegionInstanceGroupManagersDeleteInstancesRequest{
					Instances: []string{instanceURL},
				}
				_, err := svc.RegionInstanceGroupManagers.DeleteInstances(cfg.ProjectID, cfg.Region, cfg.MIGName, req).Context(ctx).Do()
				if err != nil {
					log.WithError(err).WithField("instance", h.InstanceName).Warn("Failed to delete unhealthy host")
					continue
				}
				delete(instanceByName, h.InstanceName)
				phase0Deleted++
			}
			// Mark terminated in DB.
			_ = hr.SetHostStatusByInstanceName(ctx, h.InstanceName, "terminated")
			// Clean up orphaned runners.
			if err := hr.CleanupHostRunners(ctx, h.ID); err != nil {
				log.WithError(err).WithField("host_id", h.ID).Warn("Failed to clean up runners for unhealthy host")
			}
			// Remove from in-memory map.
			hr.RemoveHost(h.ID)
			delete(hostByName, h.InstanceName)
			log.WithFields(logrus.Fields{
				"host_id":  h.ID,
				"instance": h.InstanceName,
			}).Info("Cleaned up unhealthy host")
		}

		if h.Status == "terminating" && time.Since(h.LastHeartbeat) >= unhealthyGracePeriod {
			// GCE instance already deleted; just clean up in-memory state.
			if err := hr.CleanupHostRunners(ctx, h.ID); err != nil {
				log.WithError(err).WithField("host_id", h.ID).Warn("Failed to clean up runners for terminating host")
			}
			hr.RemoveHost(h.ID)
			delete(hostByName, h.InstanceName)
			log.WithFields(logrus.Fields{
				"host_id":  h.ID,
				"instance": h.InstanceName,
			}).Info("Cleaned up stale terminating host")
		}
	}

	// Phase 1: delete draining+idle hosts via DeleteInstances.
	deleted := int64(0)
	for instanceName, instanceURL := range instanceByName {
		if deleted >= int64(cfg.MaxDeletesPerCycle) {
			break
		}
		h := hostByName[instanceName]
		if h == nil {
			continue
		}
		if h.Status != "draining" {
			continue
		}
		if time.Since(h.LastHeartbeat) > cfg.HeartbeatStaleWindow {
			continue
		}
		if h.BusyRunners != 0 {
			continue
		}

		req := &compute.RegionInstanceGroupManagersDeleteInstancesRequest{
			Instances: []string{instanceURL},
		}
		_, err := svc.RegionInstanceGroupManagers.DeleteInstances(cfg.ProjectID, cfg.Region, cfg.MIGName, req).Context(ctx).Do()
		if err != nil {
			log.WithError(err).WithField("instance", instanceName).Warn("Failed to delete draining host")
			continue
		}
		_ = hr.SetHostStatusByInstanceName(ctx, instanceName, "terminating")
		deleted++
	}

	if deleted > 0 {
		log.WithField("deleted", deleted).Info("Phase 1: deleted draining+idle hosts")
	}

	// Phase 2: utilization-based scale-up / scale-down.
	// Skip if within cooldown.
	if !lastScaleAction.IsZero() && time.Since(lastScaleAction) < cfg.Cooldown {
		return false, nil
	}

	// Collect ready hosts with valid CPU info and fresh heartbeat.
	var readyHosts []*Host
	for _, h := range hosts {
		if h.Status != "ready" {
			continue
		}
		if h.TotalCPUMillicores <= 0 {
			continue
		}
		if time.Since(h.LastHeartbeat) > cfg.HeartbeatStaleWindow {
			continue
		}
		readyHosts = append(readyHosts, h)
	}

	decision := computeAutoscaleDecision(readyHosts, sched.GetQueueDepth(), cfg.ScaleUpThreshold, cfg.ScaleDownThreshold)

	switch decision.action {
	case scaleActionUp:
		newTarget := currentTarget + 1
		_, err := svc.RegionInstanceGroupManagers.Resize(cfg.ProjectID, cfg.Region, cfg.MIGName, newTarget).Context(ctx).Do()
		if err != nil {
			return false, fmt.Errorf("scale-up resize to %d: %w", newTarget, err)
		}
		log.WithFields(logrus.Fields{
			"old_target": currentTarget,
			"new_target": newTarget,
			"reason":     decision.reason,
		}).Info("Scaled up MIG")
		return true, nil

	case scaleActionDown:
		victim := decision.drainTarget
		if err := hr.SetHostStatusByInstanceName(ctx, victim.InstanceName, "draining"); err != nil {
			log.WithError(err).WithField("instance", victim.InstanceName).Warn("Failed to mark host draining for scale-down")
			return false, nil
		}
		log.WithFields(logrus.Fields{
			"instance": victim.InstanceName,
			"reason":   decision.reason,
		}).Info("Scale-down: draining host")
		return true, nil
	}

	return false, nil
}

type scaleAction int

const (
	scaleActionNone scaleAction = iota
	scaleActionUp
	scaleActionDown
)

type autoscaleDecision struct {
	action      scaleAction
	drainTarget *Host // set when action == scaleActionDown
	reason      string
}

// computeAutoscaleDecision is the pure decision logic for scale-up / scale-down.
// It examines ready hosts' CPU utilization and queue depth to decide what to do.
func computeAutoscaleDecision(readyHosts []*Host, queueDepth int, scaleUpThreshold, scaleDownThreshold float64) autoscaleDecision {
	type hostUtil struct {
		host *Host
		xi   float64
	}

	utils := make([]hostUtil, len(readyHosts))
	for i, h := range readyHosts {
		utils[i] = hostUtil{
			host: h,
			xi:   float64(h.UsedCPUMillicores) / float64(h.TotalCPUMillicores),
		}
	}

	// --- Scale up ---
	if len(readyHosts) == 0 && queueDepth > 0 {
		return autoscaleDecision{action: scaleActionUp, reason: "no ready hosts with queued jobs"}
	}
	if len(utils) > 0 {
		minXi := utils[0].xi
		for _, u := range utils[1:] {
			if u.xi < minXi {
				minXi = u.xi
			}
		}
		if minXi > scaleUpThreshold {
			return autoscaleDecision{action: scaleActionUp, reason: fmt.Sprintf("min utilization %.2f > threshold %.2f", minXi, scaleUpThreshold)}
		}
	}

	// --- Scale down ---
	if len(readyHosts) <= 1 {
		return autoscaleDecision{action: scaleActionNone, reason: "too few hosts to scale down"}
	}

	// Sort by CreatedAt ascending (oldest first).
	sort.Slice(utils, func(i, j int) bool {
		return utils[i].host.CreatedAt.Before(utils[j].host.CreatedAt)
	})

	// min(Xi) excluding the newest host (last after sort).
	excludeNewest := utils[:len(utils)-1]
	minXi := excludeNewest[0].xi
	for _, u := range excludeNewest[1:] {
		if u.xi < minXi {
			minXi = u.xi
		}
	}

	if minXi >= scaleDownThreshold {
		return autoscaleDecision{action: scaleActionNone, reason: "utilization above scale-down threshold"}
	}

	// Pick the newest host with the lowest Xi.
	sort.Slice(utils, func(i, j int) bool {
		if !utils[i].host.CreatedAt.Equal(utils[j].host.CreatedAt) {
			return utils[i].host.CreatedAt.After(utils[j].host.CreatedAt)
		}
		return utils[i].xi < utils[j].xi
	})

	return autoscaleDecision{
		action:      scaleActionDown,
		drainTarget: utils[0].host,
		reason:      fmt.Sprintf("min utilization %.2f < threshold %.2f", minXi, scaleDownThreshold),
	}
}

func tryAdvisoryLock(ctx context.Context, db *sql.DB, key int64) (bool, error) {
	var ok bool
	if err := db.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

func advisoryUnlock(ctx context.Context, db *sql.DB, key int64) error {
	var ok bool
	return db.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&ok)
}

func instanceNameFromURL(u string) string {
	// Expected: .../zones/<zone>/instances/<name>
	parts := strings.Split(strings.TrimSpace(u), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "instances" {
			return parts[i+1]
		}
	}
	return ""
}
