package telemetry

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"

	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
)

// Client is a GCP Cloud Monitoring client for recording custom metrics.
type Client struct {
	config   Config
	client   *monitoring.MetricClient
	logger   *logrus.Entry
	resource *monitoredres.MonitoredResource

	mu          sync.Mutex
	buffer      []*monitoringpb.TimeSeries
	bufferIndex map[string]int
	done        chan struct{}
}

// NewClient creates a new telemetry Client.
// If telemetry is disabled in config, returns a no-op client.
func NewClient(ctx context.Context, config Config, logger *logrus.Logger) (*Client, error) {
	log := logger.WithField("component", "telemetry")

	if !config.Enabled {
		log.Info("Telemetry disabled")
		return &Client{config: config, logger: log}, nil
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	client, err := monitoring.NewMetricClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create monitoring client: %w", err)
	}

	// Determine resource type based on component
	resourceType := "global"
	resourceLabels := map[string]string{
		"project_id": config.ProjectID,
	}

	// Use gce_instance for host-level metrics
	if config.InstanceID != "" && config.Zone != "" {
		resourceType = "gce_instance"
		resourceLabels["instance_id"] = config.InstanceID
		resourceLabels["zone"] = config.Zone
	}

	c := &Client{
		config: config,
		client: client,
		logger: log,
		resource: &monitoredres.MonitoredResource{
			Type:   resourceType,
			Labels: resourceLabels,
		},
		buffer:      make([]*monitoringpb.TimeSeries, 0, config.BufferSize),
		bufferIndex: make(map[string]int, config.BufferSize),
		done:        make(chan struct{}),
	}

	// Start background flush loop
	go c.flushLoop(ctx)

	log.WithFields(logrus.Fields{
		"project":        config.ProjectID,
		"component":      config.Component,
		"resource_type":  resourceType,
		"flush_interval": config.FlushInterval,
	}).Info("Telemetry initialized")

	return c, nil
}

// Close shuts down the client and flushes any remaining metrics.
func (c *Client) Close() error {
	if c.client == nil {
		return nil
	}

	close(c.done)

	// Final flush
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Flush(ctx)

	return c.client.Close()
}

// flushLoop periodically flushes buffered metrics.
func (c *Client) flushLoop(ctx context.Context) {
	if c.client == nil {
		return
	}

	ticker := time.NewTicker(c.config.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ticker.C:
			c.Flush(ctx)
		}
	}
}

// Flush sends all buffered metrics to Cloud Monitoring.
func (c *Client) Flush(ctx context.Context) {
	if c.client == nil {
		return
	}

	c.mu.Lock()
	if len(c.buffer) == 0 {
		c.mu.Unlock()
		return
	}
	timeSeries := c.buffer
	c.buffer = make([]*monitoringpb.TimeSeries, 0, c.config.BufferSize)
	c.bufferIndex = make(map[string]int, c.config.BufferSize)
	c.mu.Unlock()

	// Cloud Monitoring API limits to 200 time series per request
	const batchSize = 200
	for i := 0; i < len(timeSeries); i += batchSize {
		end := i + batchSize
		if end > len(timeSeries) {
			end = len(timeSeries)
		}
		batch := timeSeries[i:end]

		req := &monitoringpb.CreateTimeSeriesRequest{
			Name:       fmt.Sprintf("projects/%s", c.config.ProjectID),
			TimeSeries: batch,
		}

		if err := c.client.CreateTimeSeries(ctx, req); err != nil {
			c.logger.WithError(err).WithField("count", len(batch)).Warn("Failed to write metrics")
		} else {
			c.logger.WithField("count", len(batch)).Debug("Flushed metrics")
		}
	}
}

// timeSeriesKey returns a string key that uniquely identifies a TimeSeries
// (metric type + sorted labels). Two entries with the same key cannot coexist
// in a single CreateTimeSeries request.
func timeSeriesKey(ts *monitoringpb.TimeSeries) string {
	key := ts.Metric.Type
	keys := make([]string, 0, len(ts.Metric.Labels))
	for k := range ts.Metric.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		key += "|" + k + "=" + ts.Metric.Labels[k]
	}
	return key
}

// metricType returns the full metric type string.
func (c *Client) metricType(name string) string {
	return c.config.MetricPrefix + "/" + name
}

// baseLabels returns labels that should be included with all metrics.
func (c *Client) baseLabels() map[string]string {
	labels := make(map[string]string)
	if c.config.Component != "" {
		labels["component"] = c.config.Component
	}
	if c.config.Environment != "" {
		labels["environment"] = c.config.Environment
	}
	return labels
}

// mergeLabels combines base labels with additional labels.
func (c *Client) mergeLabels(additional map[string]string) map[string]string {
	labels := c.baseLabels()
	for k, v := range additional {
		labels[k] = v
	}
	return labels
}

// RecordDuration records a timing metric as a DOUBLE value in seconds.
func (c *Client) RecordDuration(ctx context.Context, metric string, duration time.Duration, labels map[string]string) {
	c.RecordFloat(ctx, metric, duration.Seconds(), labels)
}

// RecordFloat records a float64 gauge metric.
func (c *Client) RecordFloat(ctx context.Context, metric string, value float64, labels map[string]string) {
	if c.client == nil {
		return
	}

	now := time.Now()
	ts := &monitoringpb.TimeSeries{
		Metric: &metricpb.Metric{
			Type:   c.metricType(metric),
			Labels: c.mergeLabels(labels),
		},
		Resource: c.resource,
		Points: []*monitoringpb.Point{{
			Interval: &monitoringpb.TimeInterval{
				EndTime: timestamppb.New(now),
			},
			Value: &monitoringpb.TypedValue{
				Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: value},
			},
		}},
	}

	c.addToBuffer(ts)
}

// RecordInt records an int64 gauge metric.
func (c *Client) RecordInt(ctx context.Context, metric string, value int64, labels map[string]string) {
	if c.client == nil {
		return
	}

	now := time.Now()
	ts := &monitoringpb.TimeSeries{
		Metric: &metricpb.Metric{
			Type:   c.metricType(metric),
			Labels: c.mergeLabels(labels),
		},
		Resource: c.resource,
		Points: []*monitoringpb.Point{{
			Interval: &monitoringpb.TimeInterval{
				EndTime: timestamppb.New(now),
			},
			Value: &monitoringpb.TypedValue{
				Value: &monitoringpb.TypedValue_Int64Value{Int64Value: value},
			},
		}},
	}

	c.addToBuffer(ts)
}

// IncrementCounter increments a counter metric by 1.
func (c *Client) IncrementCounter(ctx context.Context, metric string, labels map[string]string) {
	c.AddToCounter(ctx, metric, 1, labels)
}

// AddToCounter adds a value to a counter metric.
func (c *Client) AddToCounter(ctx context.Context, metric string, value int64, labels map[string]string) {
	if c.client == nil {
		return
	}

	now := time.Now()
	ts := &monitoringpb.TimeSeries{
		Metric: &metricpb.Metric{
			Type:   c.metricType(metric),
			Labels: c.mergeLabels(labels),
		},
		Resource:   c.resource,
		MetricKind: metricpb.MetricDescriptor_CUMULATIVE,
		Points: []*monitoringpb.Point{{
			Interval: &monitoringpb.TimeInterval{
				StartTime: timestamppb.New(now.Add(-1 * time.Minute)), // Required for cumulative
				EndTime:   timestamppb.New(now),
			},
			Value: &monitoringpb.TypedValue{
				Value: &monitoringpb.TypedValue_Int64Value{Int64Value: value},
			},
		}},
	}

	c.addToBuffer(ts)
}

// addToBuffer adds a time series to the buffer, replacing any existing entry
// with the same metric type + labels to prevent duplicate TimeSeries errors.
func (c *Client) addToBuffer(ts *monitoringpb.TimeSeries) {
	key := timeSeriesKey(ts)

	c.mu.Lock()
	if idx, exists := c.bufferIndex[key]; exists {
		// Replace the existing entry with the newer value
		c.buffer[idx] = ts
		c.mu.Unlock()
		return
	}
	c.bufferIndex[key] = len(c.buffer)
	c.buffer = append(c.buffer, ts)
	shouldFlush := len(c.buffer) >= c.config.BufferSize
	c.mu.Unlock()

	if shouldFlush {
		go c.Flush(context.Background())
	}
}

// RecordPhases records all phases from a Timer as separate metrics.
func (c *Client) RecordPhases(ctx context.Context, metricBase string, timer *Timer, extraLabels map[string]string) {
	if c.client == nil {
		return
	}

	// Record total duration
	c.RecordDuration(ctx, metricBase, timer.Total(), extraLabels)

	// Record each phase
	for _, phase := range timer.Phases() {
		labels := make(map[string]string)
		for k, v := range extraLabels {
			labels[k] = v
		}
		labels["phase"] = phase.Name
		c.RecordDuration(ctx, metricBase+"_phase", phase.Duration, labels)
	}
}

// HostMetrics records common host-level metrics.
type HostMetrics struct {
	TotalCPUMillicores int
	UsedCPUMillicores  int
	TotalMemoryMB      int
	UsedMemoryMB       int
	IdleRunners        int
	BusyRunners        int
}

// RecordHostMetrics records host-level runner metrics.
func (c *Client) RecordHostMetrics(ctx context.Context, m HostMetrics) {
	c.RecordInt(ctx, MetricHostCPUTotal, int64(m.TotalCPUMillicores), nil)
	c.RecordInt(ctx, MetricHostCPUUsed, int64(m.UsedCPUMillicores), nil)
	c.RecordInt(ctx, MetricHostMemTotal, int64(m.TotalMemoryMB), nil)
	c.RecordInt(ctx, MetricHostMemUsed, int64(m.UsedMemoryMB), nil)
	c.RecordInt(ctx, MetricHostRunnersIdle, int64(m.IdleRunners), nil)
	c.RecordInt(ctx, MetricHostRunnersBusy, int64(m.BusyRunners), nil)
}

// ChunkedMetrics holds chunked snapshot system metrics.
type ChunkedMetrics struct {
	DiskCacheSize    int64
	DiskCacheMaxSize int64
	DiskCacheItems   int
	MemCacheSize     int64
	MemCacheMaxSize  int64
	MemCacheItems    int
	PageFaults       uint64
	CacheHits        uint64
	ChunkFetches     uint64
	DiskReads        uint64
	DiskWrites       uint64
	DirtyChunks      int
}

// RecordChunkedMetrics records chunked snapshot system metrics.
func (c *Client) RecordChunkedMetrics(ctx context.Context, m ChunkedMetrics) {
	c.RecordInt(ctx, MetricDiskCacheSize, m.DiskCacheSize, nil)
	c.RecordInt(ctx, MetricDiskCacheMaxSize, m.DiskCacheMaxSize, nil)
	c.RecordInt(ctx, MetricDiskCacheItems, int64(m.DiskCacheItems), nil)
	c.RecordInt(ctx, MetricMemCacheSize, m.MemCacheSize, nil)
	c.RecordInt(ctx, MetricMemCacheMaxSize, m.MemCacheMaxSize, nil)
	c.RecordInt(ctx, MetricMemCacheItems, int64(m.MemCacheItems), nil)
	c.RecordInt(ctx, MetricChunkPageFaults, int64(m.PageFaults), nil)
	c.RecordInt(ctx, MetricChunkCacheHits, int64(m.CacheHits), nil)
	c.RecordInt(ctx, MetricChunkFetches, int64(m.ChunkFetches), nil)
	c.RecordInt(ctx, MetricChunkDiskReads, int64(m.DiskReads), nil)
	c.RecordInt(ctx, MetricChunkDiskWrites, int64(m.DiskWrites), nil)
	c.RecordInt(ctx, MetricChunkDirtyChunks, int64(m.DirtyChunks), nil)

	// Compute and record cache hit ratio
	total := m.CacheHits + m.ChunkFetches
	if total > 0 {
		ratio := float64(m.CacheHits) / float64(total)
		c.RecordFloat(ctx, MetricChunkCacheHitRatio, ratio, nil)
	}
}

// PoolMetrics holds runner pool metrics.
type PoolMetrics struct {
	PooledRunners   int
	PoolHits        int64
	PoolMisses      int64
	Evictions       int64
	RecycleFailures int64
	MemoryUsedBytes int64
	MemoryMaxBytes  int64
}

// RecordPoolMetrics records runner pool metrics.
func (c *Client) RecordPoolMetrics(ctx context.Context, m PoolMetrics) {
	c.RecordInt(ctx, MetricPoolRunners, int64(m.PooledRunners), nil)
	c.RecordInt(ctx, MetricPoolHits, m.PoolHits, nil)
	c.RecordInt(ctx, MetricPoolMisses, m.PoolMisses, nil)
	c.RecordInt(ctx, MetricPoolEvictions, m.Evictions, nil)
	c.RecordInt(ctx, MetricPoolRecycleFails, m.RecycleFailures, nil)
	c.RecordInt(ctx, MetricPoolMemoryUsed, m.MemoryUsedBytes, nil)
	c.RecordInt(ctx, MetricPoolMemoryMax, m.MemoryMaxBytes, nil)

	// Compute and record hit ratio
	total := m.PoolHits + m.PoolMisses
	if total > 0 {
		ratio := float64(m.PoolHits) / float64(total)
		c.RecordFloat(ctx, MetricPoolHitRatio, ratio, nil)
	}
}
