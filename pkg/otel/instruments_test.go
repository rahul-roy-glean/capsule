package otel

import (
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
)

func TestAllHistogramsHaveDescriptions(t *testing.T) {
	histograms := []HistogramName{
		VMBootDuration, VMReadyDuration, VMLifetime, VMJobDuration,
		HostBootDuration, HostGCSSyncDuration, HostHeartbeatLatency,
		CPWebhookLatency, CPAllocationLatency, CPQueueWait,
		SnapshotBuildDuration, SnapshotUploadDuration,
		SessionPauseDuration, SessionResumeDuration,
		CacheGitCloneDuration, GitHubRegistrationDuration, GitHubJobPickupLatency,
		UFFDFaultServiceDuration, ChunkFetchDuration,
	}
	for _, name := range histograms {
		if _, ok := histogramDescriptions[name]; !ok {
			t.Errorf("histogram %q missing description", name)
		}
		if _, ok := histogramUnits[name]; !ok {
			t.Errorf("histogram %q missing unit", name)
		}
	}
}

func TestAllCountersHaveDescriptions(t *testing.T) {
	counters := []CounterName{
		VMAllocations, VMTerminations,
		CPWebhookRequests, CPAllocations, CPDownscalerActions,
		SnapshotRollouts,
		CacheArtifactHits, CacheArtifactMisses, CacheGitClones,
		CITokenRequests, CIJobs,
		ChunkedPageFaults, ChunkedCacheHits, ChunkedChunkFetches,
		ChunkedDiskReads, ChunkedDiskWrites,
		PoolHits, PoolMisses, PoolEvictions, PoolRecycleFailures,
		SessionPauseTotal, SessionResumeTotal, SessionResumeRouting,
		E2ECanarySuccess, E2ECanaryFailure,
		ChunkFetchBytes, ChunkSingleflightDedup, ChunkNegCacheHits,
		NetworkBytesTx, NetworkBytesRx,
	}
	for _, name := range counters {
		if _, ok := counterDescriptions[name]; !ok {
			t.Errorf("counter %q missing description", name)
		}
	}
}

func TestAllGaugesHaveDescriptions(t *testing.T) {
	gauges := []GaugeName{
		HostCPUTotal, HostCPUUsed, HostMemTotal, HostMemUsed,
		CPHostsTotal, CPRunnersTotal, CPQueueDepth,
		CPFleetCPUTotal, CPFleetCPUUsed, CPFleetCPUFree,
		CPFleetMemTotal, CPFleetMemUsed, CPFleetMemFree,
		ChunkedDiskCacheSize, ChunkedDiskCacheMax, ChunkedDiskCacheItems,
		ChunkedMemCacheSize, ChunkedMemCacheMax, ChunkedMemCacheItems,
		ChunkedDirtyChunks,
		PoolRunners, PoolMemoryUsed, PoolMemoryMax,
		SnapshotSize, SnapshotAge,
		HostGCSSyncBytes, HostUptime, CacheArtifactSize, NetworkConnections,
	}
	for _, name := range gauges {
		if _, ok := gaugeDescriptions[name]; !ok {
			t.Errorf("gauge %q missing description", name)
		}
	}
}

func TestNewCounterCreatesInstrument(t *testing.T) {
	meter := noop.NewMeterProvider().Meter("test")
	counter, err := NewCounter(meter, VMAllocations)
	if err != nil {
		t.Fatalf("NewCounter returned error: %v", err)
	}
	if counter == nil {
		t.Error("NewCounter returned nil")
	}
}

func TestNewHistogramCreatesInstrument(t *testing.T) {
	meter := noop.NewMeterProvider().Meter("test")
	hist, err := NewHistogram(meter, VMBootDuration)
	if err != nil {
		t.Fatalf("NewHistogram returned error: %v", err)
	}
	if hist == nil {
		t.Error("NewHistogram returned nil")
	}
}

func TestNewGaugeCreatesInstrument(t *testing.T) {
	meter := noop.NewMeterProvider().Meter("test")
	gauge, err := NewGauge(meter, HostCPUTotal)
	if err != nil {
		t.Fatalf("NewGauge returned error: %v", err)
	}
	if gauge == nil {
		t.Error("NewGauge returned nil")
	}
}
