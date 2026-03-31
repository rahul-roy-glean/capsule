package main

import (
	"context"
	"fmt"
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

// defaultAutoscaleInput returns a baseline autoscaleInput with zero-value
// fields for rate-based logic and sensible defaults for settling/minHostAge.
func defaultAutoscaleInput(hosts []*Host, scaleUpThreshold, scaleDownThreshold float64, allocFailures int64) autoscaleInput {
	return autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   scaleUpThreshold,
		scaleDownThreshold: scaleDownThreshold,
		settlingThreshold:  0.2,
		minHostAge:         0, // no age filter by default in legacy tests
		allocFailures:      allocFailures,
		bootCooldown:       3 * time.Minute,
	}
}

func TestComputeAutoscaleDecision_ScaleUpNoReadyHosts(t *testing.T) {
	d := computeAutoscaleDecision(defaultAutoscaleInput(nil, 0.9, 0.5, 0))
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp when no ready hosts, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_ScaleUpAllHostsAboveThreshold(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 950, 1000, now.Add(-2*time.Hour)),
		makeHost("h2", 920, 1000, now.Add(-1*time.Hour)),
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
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
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
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
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown, got %d", d.action)
	}
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
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown, got %d", d.action)
	}
	if d.drainTarget.InstanceName != "h-new" {
		t.Fatalf("expected drain target h-new, got %s", d.drainTarget.InstanceName)
	}
}

func TestComputeAutoscaleDecision_ScaleDownExcludesUnsettledFromMinXi(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h-old", 600, 1000, now.Add(-2*time.Hour)), // Xi=0.6, settled (>=20%)
		makeHost("h-new", 0, 1000, now),                     // Xi=0.0, unsettled (<20%)
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionNone {
		t.Fatalf("expected scaleActionNone (unsettled excluded), got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_ScaleDownExcludesMultipleUnsettledHosts(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h-old", 600, 1000, now.Add(-1*time.Hour)),    // Xi=0.6, settled
		makeHost("h-new1", 0, 1000, now.Add(-2*time.Minute)),   // Xi=0.0, unsettled
		makeHost("h-new2", 0, 1000, now.Add(-1*time.Minute)),   // Xi=0.0, unsettled
		makeHost("h-new3", 50, 1000, now.Add(-30*time.Second)), // Xi=0.05, unsettled
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionNone {
		t.Fatalf("expected scaleActionNone when multiple unsettled hosts excluded, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_ScaleDownAllUnsettledFallsThrough(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 100, 1000, now.Add(-1*time.Hour)), // Xi=0.1
		makeHost("h2", 50, 1000, now.Add(-2*time.Hour)),  // Xi=0.05
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown when all hosts genuinely underutilized, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_NeverScaleBelowOne(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 100, 1000, now), // Xi=0.1, well below threshold
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
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
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionNone {
		t.Fatalf("expected scaleActionNone, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_ScaleUpSingleHostAboveThreshold(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 950, 1000, now), // Xi=0.95
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp for single overloaded host, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_ScaleUpExactlyAtThresholdNoScaleUp(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 900, 1000, now), // Xi=0.9, exactly at threshold (not >)
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
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
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
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
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp when all hosts above threshold, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_VictimSelectionPrefersNewest(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h-old", 200, 1000, now.Add(-3*time.Hour)), // Xi=0.2
		makeHost("h-mid", 200, 1000, now.Add(-2*time.Hour)), // Xi=0.2
		makeHost("h-new", 200, 1000, now.Add(-1*time.Hour)), // Xi=0.2
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown, got %d", d.action)
	}
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

	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.7, 0.5, 0))
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp with lower threshold, got %d", d.action)
	}

	d = computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
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

	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.3, 0))
	if d.action == scaleActionDown {
		t.Fatal("should not scale down when min Xi >= custom threshold")
	}

	d = computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 0))
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown with higher threshold, got %d", d.action)
	}
}

func TestLoadDownscalerConfig_Defaults(t *testing.T) {
	for _, key := range []string{
		"GCP_PROJECT_ID", "GCP_REGION", "HOST_MIG_NAME",
		"DOWNSCALER_ENABLED", "DOWNSCALER_INTERVAL",
		"DOWNSCALER_MAX_DELETES", "DOWNSCALER_MAX_DRAINS",
		"DOWNSCALER_HEARTBEAT_STALE",
		"AUTOSCALER_SCALE_UP_THRESHOLD", "AUTOSCALER_SCALE_DOWN_THRESHOLD",
		"AUTOSCALER_COOLDOWN", "AUTOSCALER_RATE_WINDOW",
		"AUTOSCALER_SETTLING_THRESHOLD", "AUTOSCALER_MIN_HOST_AGE",
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
	if cfg.RateWindow != 60*time.Second {
		t.Errorf("expected RateWindow 60s, got %s", cfg.RateWindow)
	}
	if cfg.SettlingThreshold != 0.2 {
		t.Errorf("expected SettlingThreshold 0.2, got %f", cfg.SettlingThreshold)
	}
	if cfg.MinHostAge != 10*time.Minute {
		t.Errorf("expected MinHostAge 10m, got %s", cfg.MinHostAge)
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
	t.Setenv("AUTOSCALER_SETTLING_THRESHOLD", "0.15")
	t.Setenv("AUTOSCALER_MIN_HOST_AGE", "5m")
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
	if cfg.SettlingThreshold != 0.15 {
		t.Errorf("expected SettlingThreshold 0.15, got %f", cfg.SettlingThreshold)
	}
	if cfg.MinHostAge != 5*time.Minute {
		t.Errorf("expected MinHostAge 5m, got %s", cfg.MinHostAge)
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

type mockMIGClient struct {
	targetSize       int64
	instances        map[string]string // name → URL
	deletedInstances []string          // URLs passed to DeleteInstances
	resizedTo        int64             // last Resize target (0 means not called)
	deleteErr        error
	resizeErr        error
}

func (m *mockMIGClient) GetTargetSize(_ context.Context) (int64, error) {
	return m.targetSize, nil
}

func (m *mockMIGClient) ListInstances(_ context.Context) (map[string]string, error) {
	cp := make(map[string]string, len(m.instances))
	for k, v := range m.instances {
		cp[k] = v
	}
	return cp, nil
}

func (m *mockMIGClient) DeleteInstances(_ context.Context, urls []string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deletedInstances = append(m.deletedInstances, urls...)
	return nil
}

func (m *mockMIGClient) Resize(_ context.Context, newSize int64) error {
	if m.resizeErr != nil {
		return m.resizeErr
	}
	m.resizedTo = newSize
	return nil
}

type mockHostStore struct {
	hosts          []*Host
	statusUpdates  map[string]string // instanceName → last status set
	removedHosts   []string          // host IDs removed
	cleanedUp      []string          // host IDs cleaned up
	allocFailures  int64
	allocationRate float64
}

func newMockHostStore(hosts []*Host) *mockHostStore {
	return &mockHostStore{
		hosts:         hosts,
		statusUpdates: map[string]string{},
	}
}

func (m *mockHostStore) GetAllHosts() []*Host {
	return m.hosts
}

func (m *mockHostStore) SetHostStatusByInstanceName(_ context.Context, instanceName, status string) error {
	m.statusUpdates[instanceName] = status
	return nil
}

func (m *mockHostStore) CleanupHostRunners(_ context.Context, hostID string) error {
	m.cleanedUp = append(m.cleanedUp, hostID)
	return nil
}

func (m *mockHostStore) RemoveHost(hostID string) {
	m.removedHosts = append(m.removedHosts, hostID)
}

func (m *mockHostStore) DrainAllocFailures() int64 {
	v := m.allocFailures
	m.allocFailures = 0
	return v
}

func (m *mockHostStore) GetAllocationRate() float64 {
	return m.allocationRate
}

func defaultTestConfig() downscalerConfig {
	return downscalerConfig{
		MaxDeletesPerCycle:   5,
		MaxDrainsPerCycle:    5,
		HeartbeatStaleWindow: 90 * time.Second,
		ScaleUpThreshold:     0.9,
		ScaleDownThreshold:   0.5,
		Cooldown:             5 * time.Minute,
		BootCooldown:         3 * time.Minute,
		RateWindow:           60 * time.Second,
		SettlingThreshold:    0.2,
		MinHostAge:           10 * time.Minute,
	}
}

func TestRunDownscaleOnce_ScaleUpResizesMIG(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h1", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 950, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
		{ID: "2", InstanceName: "h2", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 920, CreatedAt: now.Add(-1 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 2, instances: map[string]string{"h1": "url/h1", "h2": "url/h2"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acted {
		t.Fatal("expected action taken")
	}
	if mc.resizedTo != 3 {
		t.Fatalf("expected MIG resized to 3, got %d", mc.resizedTo)
	}
}

func TestRunDownscaleOnce_ScaleDownDrainsVictim(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h-old", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 300, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
		{ID: "2", InstanceName: "h-new", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 200, CreatedAt: now.Add(-1 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 2, instances: map[string]string{"h-old": "url/h-old", "h-new": "url/h-new"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acted {
		t.Fatal("expected action taken")
	}
	status, ok := hs.statusUpdates["h-new"]
	if !ok {
		t.Fatal("expected h-new status to be updated")
	}
	if status != "draining" {
		t.Fatalf("expected h-new status 'draining', got '%s'", status)
	}
	if mc.resizedTo != 0 {
		t.Fatalf("expected no MIG resize on scale-down, got resize to %d", mc.resizedTo)
	}
}

func TestRunDownscaleOnce_Phase1DeletesDrainingIdleHosts(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h-draining", Status: "draining", BusyRunners: 0, TotalCPUMillicores: 1000, UsedCPUMillicores: 0, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
		{ID: "2", InstanceName: "h-ready", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 700, CreatedAt: now.Add(-1 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 2, instances: map[string]string{"h-draining": "url/h-draining", "h-ready": "url/h-ready"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	_, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.deletedInstances) != 1 || mc.deletedInstances[0] != "url/h-draining" {
		t.Fatalf("expected h-draining deleted, got %v", mc.deletedInstances)
	}
	if hs.statusUpdates["h-draining"] != "terminating" {
		t.Fatalf("expected h-draining status 'terminating', got '%s'", hs.statusUpdates["h-draining"])
	}
}

func TestRunDownscaleOnce_Phase1SkipsBusyDrainingHosts(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h-busy", Status: "draining", BusyRunners: 3, TotalCPUMillicores: 1000, UsedCPUMillicores: 500, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
		{ID: "2", InstanceName: "h-ready", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 700, CreatedAt: now.Add(-1 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 2, instances: map[string]string{"h-busy": "url/h-busy", "h-ready": "url/h-ready"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	_, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.deletedInstances) != 0 {
		t.Fatalf("expected no deletions for busy draining host, got %v", mc.deletedInstances)
	}
}

func TestRunDownscaleOnce_Phase0CleansUpUnhealthyHosts(t *testing.T) {
	staleTime := time.Now().Add(-10 * time.Minute)
	hosts := []*Host{
		{ID: "unhealthy-1", InstanceName: "h-unhealthy", Status: "unhealthy", LastHeartbeat: staleTime, CreatedAt: staleTime},
	}

	mc := &mockMIGClient{targetSize: 1, instances: map[string]string{"h-unhealthy": "url/h-unhealthy"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	_, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.deletedInstances) != 1 || mc.deletedInstances[0] != "url/h-unhealthy" {
		t.Fatalf("expected h-unhealthy deleted, got %v", mc.deletedInstances)
	}
	if hs.statusUpdates["h-unhealthy"] != "terminated" {
		t.Fatalf("expected status 'terminated', got '%s'", hs.statusUpdates["h-unhealthy"])
	}
	if len(hs.cleanedUp) != 1 || hs.cleanedUp[0] != "unhealthy-1" {
		t.Fatalf("expected cleanup for unhealthy-1, got %v", hs.cleanedUp)
	}
	if len(hs.removedHosts) != 1 || hs.removedHosts[0] != "unhealthy-1" {
		t.Fatalf("expected removal of unhealthy-1, got %v", hs.removedHosts)
	}
}

func TestRunDownscaleOnce_Phase0CleansUpStaleTerminatingHosts(t *testing.T) {
	staleTime := time.Now().Add(-10 * time.Minute)
	hosts := []*Host{
		{ID: "term-1", InstanceName: "h-terminating", Status: "terminating", LastHeartbeat: staleTime, CreatedAt: staleTime},
	}

	mc := &mockMIGClient{targetSize: 1, instances: map[string]string{}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	_, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.deletedInstances) != 0 {
		t.Fatalf("expected no MIG deletions for terminating host, got %v", mc.deletedInstances)
	}
	if len(hs.cleanedUp) != 1 || hs.cleanedUp[0] != "term-1" {
		t.Fatalf("expected cleanup for term-1, got %v", hs.cleanedUp)
	}
	if len(hs.removedHosts) != 1 || hs.removedHosts[0] != "term-1" {
		t.Fatalf("expected removal of term-1, got %v", hs.removedHosts)
	}
}

func TestRunDownscaleOnce_CooldownSkipsPhase2(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h1", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 950, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 1, instances: map[string]string{"h1": "url/h1"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	lastAction := now.Add(-1 * time.Minute)
	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, lastAction, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acted {
		t.Fatal("expected no action during cooldown")
	}
	if mc.resizedTo != 0 {
		t.Fatalf("expected no resize during cooldown, got %d", mc.resizedTo)
	}
}

func TestRunDownscaleOnce_CooldownExpiredAllowsAction(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h1", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 950, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 1, instances: map[string]string{"h1": "url/h1"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	lastAction := now.Add(-10 * time.Minute)
	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, lastAction, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acted {
		t.Fatal("expected action after cooldown expired")
	}
	if mc.resizedTo != 2 {
		t.Fatalf("expected resize to 2, got %d", mc.resizedTo)
	}
}

func TestRunDownscaleOnce_NoActionMidUtilization(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h1", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 700, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
		{ID: "2", InstanceName: "h2", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 600, CreatedAt: now.Add(-1 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 2, instances: map[string]string{"h1": "url/h1", "h2": "url/h2"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acted {
		t.Fatal("expected no action for mid-utilization hosts")
	}
	if mc.resizedTo != 0 {
		t.Fatal("expected no resize")
	}
	if len(hs.statusUpdates) != 0 {
		t.Fatalf("expected no status updates, got %v", hs.statusUpdates)
	}
}

func TestRunDownscaleOnce_StaleHeartbeatHostsExcludedFromReadyHosts(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h-stale", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 950, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now.Add(-5 * time.Minute)},
		{ID: "2", InstanceName: "h-fresh", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 700, CreatedAt: now.Add(-1 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 2, instances: map[string]string{"h-stale": "url/h-stale", "h-fresh": "url/h-fresh"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acted {
		t.Fatal("expected no action when stale host excluded leaves single ready host")
	}
}

func TestRunDownscaleOnce_ResizeErrorReturnsError(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h1", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 950, CreatedAt: now, LastHeartbeat: now},
	}

	mc := &mockMIGClient{
		targetSize: 1,
		instances:  map[string]string{"h1": "url/h1"},
		resizeErr:  fmt.Errorf("quota exceeded"),
	}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err == nil {
		t.Fatal("expected error from resize failure")
	}
	if acted {
		t.Fatal("expected no action on error")
	}
}

func TestComputeAutoscaleDecision_ScaleUpOnAllocFailures(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 300, 1000, now),
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 5))
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp on alloc failures, got %d", d.action)
	}
	if d.reason != "5 allocation failures since last check" {
		t.Fatalf("unexpected reason: %s", d.reason)
	}
}

func TestComputeAutoscaleDecision_ScaleUpOnZeroReadyHosts(t *testing.T) {
	d := computeAutoscaleDecision(defaultAutoscaleInput(nil, 0.9, 0.5, 0))
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp with zero ready hosts, got %d", d.action)
	}
	if d.reason != "no ready hosts available" {
		t.Fatalf("unexpected reason: %s", d.reason)
	}

	d = computeAutoscaleDecision(defaultAutoscaleInput([]*Host{}, 0.9, 0.5, 0))
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp with empty ready hosts, got %d", d.action)
	}
}

func TestComputeAutoscaleDecision_AllocFailuresTakePriority(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 700, 1000, now.Add(-2*time.Hour)),
		makeHost("h2", 600, 1000, now.Add(-1*time.Hour)),
	}
	d := computeAutoscaleDecision(defaultAutoscaleInput(hosts, 0.9, 0.5, 3))
	if d.action != scaleActionUp {
		t.Fatalf("expected scaleActionUp from alloc failures overriding mid-util, got %d", d.action)
	}
}

func TestRunDownscaleOnce_DemandDrivenRespectsBootCooldown(t *testing.T) {
	now := time.Now()

	hosts := []*Host{
		{ID: "1", InstanceName: "h1", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 300, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
	}
	mc := &mockMIGClient{targetSize: 1, instances: map[string]string{"h1": "url/h1"}}
	hs := newMockHostStore(hosts)
	hs.allocFailures = 2
	log := newTestLogger().WithField("test", true)

	lastAction := now.Add(-1 * time.Minute)
	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, lastAction, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acted {
		t.Fatal("expected demand-driven suppressed within boot cooldown")
	}

	mc2 := &mockMIGClient{targetSize: 1, instances: map[string]string{"h1": "url/h1"}}
	hs2 := newMockHostStore(hosts)
	hs2.allocFailures = 2
	lastAction2 := now.Add(-4 * time.Minute)
	acted2, err2 := runDownscaleOnce(context.Background(), defaultTestConfig(), mc2, hs2, lastAction2, log)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if !acted2 {
		t.Fatal("expected demand-driven to proceed after boot cooldown expired")
	}
	if mc2.resizedTo != 2 {
		t.Fatalf("expected MIG resized to 2, got %d", mc2.resizedTo)
	}
}

func TestRunDownscaleOnce_ZeroReadyHostsRespectsBootCooldown(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		{ID: "1", InstanceName: "h1", Status: "draining", BusyRunners: 1, TotalCPUMillicores: 1000, UsedCPUMillicores: 500, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 1, instances: map[string]string{"h1": "url/h1"}}
	hs := newMockHostStore(hosts)
	log := newTestLogger().WithField("test", true)

	lastAction := now.Add(-1 * time.Minute)
	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, lastAction, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acted {
		t.Fatal("expected suppressed within boot cooldown even with zero ready hosts")
	}

	mc2 := &mockMIGClient{targetSize: 1, instances: map[string]string{"h1": "url/h1"}}
	hs2 := newMockHostStore(hosts)
	lastAction2 := now.Add(-4 * time.Minute)
	acted2, err2 := runDownscaleOnce(context.Background(), defaultTestConfig(), mc2, hs2, lastAction2, log)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if !acted2 {
		t.Fatal("expected action after boot cooldown expired with zero ready hosts")
	}
	if mc2.resizedTo != 2 {
		t.Fatalf("expected MIG resized to 2, got %d", mc2.resizedTo)
	}
}

// --- New rate-based and MinHostAge tests ---

func TestComputeAutoscaleDecision_RateBasedScaleUp(t *testing.T) {
	now := time.Now()
	// 2 hosts, 200 mCPU spare each = 400 remaining. Rate=5 mCPU/s → TTE=80s < 180s boot
	hosts := []*Host{
		makeHost("h1", 800, 1000, now.Add(-2*time.Hour)),
		makeHost("h2", 800, 1000, now.Add(-1*time.Hour)),
	}
	d := computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.2,
		allocFailures:      0,
		allocationRateCPU:  5.0,
		remainingCapacity:  400,
		bootCooldown:       3 * time.Minute,
		avgCapacityPerHost: 1000,
	})
	if d.action != scaleActionUp {
		t.Fatalf("expected rate-based scaleActionUp, got %d", d.action)
	}
	if d.scaleUpBy < 1 {
		t.Fatalf("expected scaleUpBy >= 1, got %d", d.scaleUpBy)
	}
}

func TestComputeAutoscaleDecision_RateBasedMultiHostScaleUp(t *testing.T) {
	now := time.Now()
	// 1 host, 100 mCPU remaining. Rate=20 mCPU/s → TTE=5s << 2×180s=360s headroom
	// deficit = 20*360 - 100 = 7100. hosts = ceil(7100/1000) = 8
	hosts := []*Host{
		makeHost("h1", 900, 1000, now.Add(-2*time.Hour)),
	}
	d := computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.2,
		allocFailures:      0,
		allocationRateCPU:  20.0,
		remainingCapacity:  100,
		bootCooldown:       3 * time.Minute,
		avgCapacityPerHost: 1000,
	})
	if d.action != scaleActionUp {
		t.Fatalf("expected rate-based scaleActionUp, got %d", d.action)
	}
	if d.scaleUpBy != 8 {
		t.Fatalf("expected scaleUpBy=8 for multi-host (2x headroom), got %d", d.scaleUpBy)
	}
}

func TestComputeAutoscaleDecision_RateBasedNoScaleUpSufficientCapacity(t *testing.T) {
	now := time.Now()
	// 3 hosts, 800 mCPU spare each = 2400 remaining. Rate=2 mCPU/s → TTE=1200s >> 180s
	hosts := []*Host{
		makeHost("h1", 200, 1000, now.Add(-3*time.Hour)),
		makeHost("h2", 200, 1000, now.Add(-2*time.Hour)),
		makeHost("h3", 200, 1000, now.Add(-1*time.Hour)),
	}
	d := computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.2,
		allocFailures:      0,
		allocationRateCPU:  2.0,
		remainingCapacity:  2400,
		bootCooldown:       3 * time.Minute,
		avgCapacityPerHost: 1000,
	})
	if d.action == scaleActionUp {
		t.Fatal("should not scale up when TTE >> boot cooldown")
	}
}

func TestComputeAutoscaleDecision_RateBasedZeroRate(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 500, 1000, now.Add(-1*time.Hour)),
	}
	d := computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.2,
		allocFailures:      0,
		allocationRateCPU:  0,
		remainingCapacity:  500,
		bootCooldown:       3 * time.Minute,
		avgCapacityPerHost: 1000,
	})
	// Zero rate should not trigger rate-based scale-up
	if d.action == scaleActionUp {
		t.Fatal("should not rate-based scale up with zero allocation rate")
	}
}

func TestComputeAutoscaleDecision_MinHostAgeProtectsDrain(t *testing.T) {
	now := time.Now()
	// 3 hosts: 30m old, 15m old, 5m old. MinHostAge=10m. Low utilization → scale down.
	hosts := []*Host{
		makeHost("h-30m", 200, 1000, now.Add(-30*time.Minute)),
		makeHost("h-15m", 200, 1000, now.Add(-15*time.Minute)),
		makeHost("h-5m", 200, 1000, now.Add(-5*time.Minute)),
	}
	d := computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.2,
		minHostAge:         10 * time.Minute,
		allocFailures:      0,
		bootCooldown:       3 * time.Minute,
	})
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown, got %d", d.action)
	}
	// h-5m should be excluded (too young). Victim should be h-15m (newest eligible).
	if d.drainTarget.InstanceName != "h-15m" {
		t.Fatalf("expected drain target h-15m (newest eligible), got %s", d.drainTarget.InstanceName)
	}
}

func TestComputeAutoscaleDecision_MinHostAgeAllTooYoung(t *testing.T) {
	now := time.Now()
	hosts := []*Host{
		makeHost("h1", 100, 1000, now.Add(-3*time.Minute)),
		makeHost("h2", 100, 1000, now.Add(-2*time.Minute)),
	}
	d := computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.2,
		minHostAge:         10 * time.Minute,
		allocFailures:      0,
		bootCooldown:       3 * time.Minute,
	})
	if d.action != scaleActionNone {
		t.Fatalf("expected scaleActionNone when all hosts too young, got %d", d.action)
	}
	if d.reason != "all hosts too young for drain" {
		t.Fatalf("unexpected reason: %s", d.reason)
	}
}

func TestComputeAutoscaleDecision_SettlingThresholdConfigurable(t *testing.T) {
	now := time.Now()
	// h-old at 0.15 — below default 0.2 settling but above custom 0.1
	hosts := []*Host{
		makeHost("h-old", 150, 1000, now.Add(-2*time.Hour)),
		makeHost("h-new", 600, 1000, now.Add(-1*time.Hour)),
	}

	// With settling=0.2, h-old (0.15) is unsettled. Settled={h-new(0.6)}, min=0.6 >= 0.5 → no scale-down.
	d := computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.2,
		bootCooldown:       3 * time.Minute,
	})
	if d.action == scaleActionDown {
		t.Fatal("should not scale down with default settling threshold excluding h-old")
	}

	// With settling=0.1, h-old (0.15) is settled. Settled={h-old(0.15), h-new(0.6)}, min=0.15 < 0.5 → scale-down.
	d = computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.1,
		bootCooldown:       3 * time.Minute,
	})
	if d.action != scaleActionDown {
		t.Fatalf("expected scaleActionDown with lower settling threshold, got %d", d.action)
	}
}

func TestRunDownscaleOnce_RateBasedMultiHostResize(t *testing.T) {
	now := time.Now()
	// 1 host with 100 mCPU remaining, rate=20 mCPU/s → needs 4 new hosts
	hosts := []*Host{
		{ID: "1", InstanceName: "h1", Status: "ready", TotalCPUMillicores: 1000, UsedCPUMillicores: 900, CreatedAt: now.Add(-2 * time.Hour), LastHeartbeat: now},
	}

	mc := &mockMIGClient{targetSize: 1, instances: map[string]string{"h1": "url/h1"}}
	hs := newMockHostStore(hosts)
	hs.allocationRate = 20.0
	log := newTestLogger().WithField("test", true)

	acted, err := runDownscaleOnce(context.Background(), defaultTestConfig(), mc, hs, time.Time{}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acted {
		t.Fatal("expected action taken for rate-based scale-up")
	}
	// Rate=20, remaining=100, TTE=5s < 2×boot=360s.
	// deficit=20*360-100=7100, hosts=ceil(7100/1000)=8
	if mc.resizedTo != 9 { // 1 + 8
		t.Fatalf("expected MIG resized to 9 (1+8), got %d", mc.resizedTo)
	}
}

func TestComputeAutoscaleDecision_RateBasedSkipsLowUtilization(t *testing.T) {
	now := time.Now()
	// Host at 5% utilization with high allocation rate. TTE would trigger
	// rate-based scaling, but utilization is below settlingThreshold/2 (10%)
	// so the rate-based check should be skipped.
	hosts := []*Host{
		makeHost("h1", 50, 1000, now.Add(-2*time.Hour)),
	}
	d := computeAutoscaleDecision(autoscaleInput{
		readyHosts:         hosts,
		scaleUpThreshold:   0.9,
		scaleDownThreshold: 0.5,
		settlingThreshold:  0.2,
		allocFailures:      0,
		allocationRateCPU:  50.0, // very high rate
		remainingCapacity:  950,  // lots of spare
		bootCooldown:       3 * time.Minute,
		avgCapacityPerHost: 1000,
	})
	// avgUtil = 1 - 950/(1*1000) = 0.05 < 0.1 (settlingThreshold/2) → rate-based skipped
	if d.action == scaleActionUp {
		t.Fatal("should not rate-based scale up when utilization is below settling threshold")
	}
}
