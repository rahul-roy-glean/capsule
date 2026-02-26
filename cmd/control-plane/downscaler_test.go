package main

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func makeHost(name string, usedCPU, totalCPU int, createdAt time.Time) *Host {
	return &Host{
		InstanceName:       name,
		Status:             "ready",
		TotalCPUMillicores: totalCPU,
		UsedCPUMillicores:  usedCPU,
		CreatedAt:          createdAt,
		LastHeartbeat:      time.Now(),
	}
}

func TestComputeAutoscaleDecision_ScaleUpNoReadyHostsWithQueue(t *testing.T) {
	d := computeAutoscaleDecision(nil, 5, 0.9, 0.5)
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_NoActionNoReadyHostsNoQueue(t *testing.T) {
	d := computeAutoscaleDecision(nil, 0, 0.9, 0.5)
	if d.action != scaleActionNone {
		t.Fatalf("expected scaleActionNone, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_ScaleUpAllHostsAboveThreshold(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 950, 1000, now.Add(-2*time.Hour)),
		makeHost("h2", 920, 1000, now.Add(-1*time.Hour)),
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_NoScaleUpWhenOneHostBelowThreshold(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 950, 1000, now.Add(-2*time.Hour)),
		makeHost("h2", 800, 1000, now.Add(-1*time.Hour)),
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action == scaleActionUp {
		t.Fatal("should not scale up when one host is below threshold")
	}
}

func TestComputeAutoscaleDecision_ScaleDownLowUtilization(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 300, 1000, now.Add(-2*time.Hour)), // Xi=0.3
		makeHost("h2", 600, 1000, now.Add(-1*time.Hour)), // Xi=0.6
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown, got %d", d.action)
	}
	// Should drain h2 (newest) since h1 (oldest, Xi=0.3) triggers threshold
	// but victim selection picks newest with lowest Xi — h2 is newer.
	if d.drainTarget.InstanceName != "h2" {
		t.Fatalf("expected drain target h2, got %s", d.drainTarget.InstanceName)
	}
}

func TestComputeAutoscaleDecision_ScaleDownPicksNewestLowestXi(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h-old", 200, 1000, now.Add(-3*time.Hour)), // Xi=0.2
		makeHost("h-mid", 400, 1000, now.Add(-2*time.Hour)), // Xi=0.4
		makeHost("h-new", 100, 1000, now.Add(-1*time.Hour)), // Xi=0.1 (newest)
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown, got %d", d.action)
	}
	// Newest host (h-new) is excluded from min(Xi) calculation.
	// min(Xi) of {h-old(0.2), h-mid(0.4)} = 0.2 < 0.5 → scale down.
	// Victim: sorted by newest first, then lowest Xi → h-new (newest, lowest Xi).
	if d.drainTarget.InstanceName != "h-new" {
		t.Fatalf("expected drain target h-new, got %s", d.drainTarget.InstanceName)
	}
}

func TestComputeAutoscaleDecision_ScaleDownExcludesNewestFromMinXi(t *testing.T) {
	now := time.Now()
	// The oldest host has high utilization, a new empty host just joined.
	// Without excluding newest, min(Xi)=0.0 would always trigger scale-down.
	// But the point is: the *newest* host is excluded from the threshold check.
	hosts := []*Host{
		makeHost("h-old", 600, 1000, now.Add(-2*time.Hour)), // Xi=0.6
		makeHost("h-new", 0, 1000, now),                     // Xi=0.0 (newest, excluded)
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	// min(Xi) excluding newest = 0.6 >= 0.5 → no scale-down
	if d.action != scaleActionNone {
		t.Fatalf("expected scaleActionNone (newest excluded), got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_NeverScaleBelowOne(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 100, 1000, now), // Xi=0.1, well below threshold
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action == scaleActionDown {
		t.Fatal("should never scale down to 0 hosts")
	}
}

func TestComputeAutoscaleDecision_NoActionMidUtilization(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 700, 1000, now.Add(-2*time.Hour)), // Xi=0.7
		makeHost("h2", 600, 1000, now.Add(-1*time.Hour)), // Xi=0.6
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	// min(Xi) excluding newest(h2) = 0.7 >= 0.5 → no scale down
	// min(Xi) overall = 0.6, not > 0.9 → no scale up
	if d.action != scaleActionNone {
		t.Fatalf("expected scaleActionNone, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_ScaleUpSingleHostAboveThreshold(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 950, 1000, now), // Xi=0.95
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp for single overloaded host, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_ScaleUpExactlyAtThresholdNoScaleUp(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 900, 1000, now), // Xi=0.9, exactly at threshold (not >)
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action == scaleActionUp {
		t.Fatal("should not scale up when Xi equals threshold (need >)")
	}
}

func TestComputeAutoscaleDecision_ScaleDownExactlyAtThresholdNoScaleDown(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 500, 1000, now.Add(-2*time.Hour)), // Xi=0.5, exactly at threshold (need <)
		makeHost("h2", 700, 1000, now.Add(-1*time.Hour)), // Xi=0.7
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action == scaleActionDown {
		t.Fatal("should not scale down when min Xi equals threshold (need <)")
	}
}

func TestComputeAutoscaleDecision_MultipleHostsAllHigh(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 950, 1000, now.Add(-3*time.Hour)),
		makeHost("h2", 960, 1000, now.Add(-2*time.Hour)),
		makeHost("h3", 970, 1000, now.Add(-1*time.Hour)),
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp when all hosts above threshold, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_VictimSelectionPrefersNewest(t *testing.T) {
	now := time.Now()
	// Two hosts with same low Xi, different ages.
	hosts := []*Host{
		makeHost("h-old", 200, 1000, now.Add(-3*time.Hour)), // Xi=0.2
		makeHost("h-mid", 200, 1000, now.Add(-2*time.Hour)), // Xi=0.2
		makeHost("h-new", 200, 1000, now.Add(-1*time.Hour)), // Xi=0.2
	}
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown, got %d", d.action)
	}
	// Should pick h-new (newest) as victim.
	if d.drainTarget.InstanceName != "h-new" {
		t.Fatalf("expected drain target h-new (newest), got %s", d.drainTarget.InstanceName)
	}
}

func TestComputeAutoscaleDecision_CustomThresholds(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 750, 1000, now.Add(-2*time.Hour)), // Xi=0.75
		makeHost("h2", 800, 1000, now.Add(-1*time.Hour)), // Xi=0.80
	}

	// With threshold 0.7, min(Xi)=0.75 > 0.7 → scale up.
	d := computeAutoscaleDecision(hosts, 0, 0.7, 0.5)
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp with lower threshold, got %d", d.action)
	}

	// With threshold 0.9, min(Xi)=0.75 not > 0.9 → no scale up.
	d = computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action == scaleActionUp {
		t.Fatal("should not scale up with default threshold")
	}
}

func TestComputeAutoscaleDecision_CustomScaleDownThreshold(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 350, 1000, now.Add(-2*time.Hour)), // Xi=0.35
		makeHost("h2", 600, 1000, now.Add(-1*time.Hour)), // Xi=0.6
	}

	// With scale-down threshold 0.3, min(Xi excluding newest)=0.35 >= 0.3 → no scale down.
	d := computeAutoscaleDecision(hosts, 0, 0.9, 0.3)
	if d.action == scaleActionDown {
		t.Fatal("should not scale down when min Xi >= custom threshold")
	}

	// With scale-down threshold 0.5, min(Xi excluding newest)=0.35 < 0.5 → scale down.
	d = computeAutoscaleDecision(hosts, 0, 0.9, 0.5)
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown with higher threshold, got %d", d.action)
	}
}

func TestLoadDownscalerConfig_Defaults(t *testing.T) {
	// Ensure defaults are sane when no env vars are set.
	// Clear relevant env vars.
	for _, key := range []string{
		"GCP_PROJECT_ID", "GCP_REGION", "HOST_MIG_NAME",
		"DOWNSCALER_ENABLED", "DOWNSCALER_INTERVAL",
		"DOWNSCALER_MAX_DELETES", "DOWNSCALER_MAX_DRAINS",
		"DOWNSCALER_HEARTBEAT_STALE",
		"AUTOSCALER_SCALE_UP_THRESHOLD", "AUTOSCALER_SCALE_DOWN_THRESHOLD",
		"AUTOSCALER_COOLDOWN",
	} {
		t.Setenv(key, "")
	}

	logger := newTestLogger()
	cfg := loadDownscalerConfig(logger.WithField("test", true))

	if cfg.ScaleUpThreshold != 0.9 {
		t.Errorf("expected ScaleUpThreshold 0.9, got %f", cfg.ScaleUpThreshold)
	}
	if cfg.ScaleDownThreshold != 0.5 {
		t.Errorf("expected ScaleDownThreshold 0.5, got %f", cfg.ScaleDownThreshold)
	}
	if cfg.Cooldown != 5*time.Minute {
		t.Errorf("expected Cooldown 5m, got %s", cfg.Cooldown)
	}
	if cfg.Enabled {
		t.Error("expected Enabled=false when no env vars set")
	}
}

func TestLoadDownscalerConfig_CustomEnvVars(t *testing.T) {
	t.Setenv("GCP_PROJECT_ID", "test-project")
	t.Setenv("GCP_REGION", "us-central1")
	t.Setenv("HOST_MIG_NAME", "test-mig")
	t.Setenv("AUTOSCALER_SCALE_UP_THRESHOLD", "0.85")
	t.Setenv("AUTOSCALER_SCALE_DOWN_THRESHOLD", "0.3")
	t.Setenv("AUTOSCALER_COOLDOWN", "10m")
	t.Setenv("DOWNSCALER_ENABLED", "")

	logger := newTestLogger()
	cfg := loadDownscalerConfig(logger.WithField("test", true))

	if cfg.ScaleUpThreshold != 0.85 {
		t.Errorf("expected ScaleUpThreshold 0.85, got %f", cfg.ScaleUpThreshold)
	}
	if cfg.ScaleDownThreshold != 0.3 {
		t.Errorf("expected ScaleDownThreshold 0.3, got %f", cfg.ScaleDownThreshold)
	}
	if cfg.Cooldown != 10*time.Minute {
		t.Errorf("expected Cooldown 10m, got %s", cfg.Cooldown)
	}
	if !cfg.Enabled {
		t.Error("expected Enabled=true when project/region/mig are set")
	}
}

func TestLoadDownscalerConfig_InvalidThresholdIgnored(t *testing.T) {
	t.Setenv("AUTOSCALER_SCALE_UP_THRESHOLD", "not-a-number")
	t.Setenv("AUTOSCALER_SCALE_DOWN_THRESHOLD", "1.5") // out of range
	t.Setenv("AUTOSCALER_COOLDOWN", "invalid")

	logger := newTestLogger()
	cfg := loadDownscalerConfig(logger.WithField("test", true))

	if cfg.ScaleUpThreshold != 0.9 {
		t.Errorf("expected default ScaleUpThreshold 0.9, got %f", cfg.ScaleUpThreshold)
	}
	if cfg.ScaleDownThreshold != 0.5 {
		t.Errorf("expected default ScaleDownThreshold 0.5, got %f", cfg.ScaleDownThreshold)
	}
	if cfg.Cooldown != 5*time.Minute {
		t.Errorf("expected default Cooldown 5m, got %s", cfg.Cooldown)
	}
}

func newTestLogger() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.WarnLevel)
	return l
}
