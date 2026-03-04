package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Host represents a Firecracker host
type Host struct {
	ID               string
	InstanceName     string
	Zone             string
	Status           string
	IdleRunners      int
	BusyRunners      int
	SnapshotVersion  string
	SnapshotSyncedAt time.Time
	LastHeartbeat    time.Time
	GRPCAddress      string
	HTTPAddress      string
	CreatedAt        time.Time
	// LoadedManifests tracks which chunked snapshot manifests are loaded per workload key (workload_key → version)
	LoadedManifests map[string]string
	// DiskUsage is the reported disk usage percentage (0.0-1.0)
	DiskUsage float64
	// Resource tracking for bin-packing scheduler
	TotalCPUMillicores int
	UsedCPUMillicores  int
	TotalMemoryMB      int
	UsedMemoryMB       int
	// RunnerInfos is per-runner status from the latest heartbeat, used for
	// centralized TTL enforcement.
	RunnerInfos []HostRunnerInfo
}

// HostRunnerInfo is per-runner status reported by a host heartbeat.
type HostRunnerInfo struct {
	RunnerID    string
	State       string
	WorkloadKey string
	IdleSince   time.Time // zero if not idle or never executed
}

// Runner represents a runner instance
type Runner struct {
	ID             string
	HostID         string
	Status         string
	InternalIP     string
	JobID          string
	WorkloadKey    string
	CreatedAt      time.Time
	StartedAt      time.Time
	CompletedAt    time.Time
	// ReservedCPU and ReservedMemoryMB track the optimistic resource reservation
	// made at allocate time, so ReleaseRunner can decrement them exactly.
	ReservedCPU      int
	ReservedMemoryMB int
}

// HostRegistry manages host registration and tracking
type HostRegistry struct {
	db      *sql.DB
	hosts   map[string]*Host
	runners map[string]*Runner
	mu      sync.RWMutex
	logger  *logrus.Entry
}

// NewHostRegistry creates a new host registry
func NewHostRegistry(db *sql.DB, logger *logrus.Logger) *HostRegistry {
	return &HostRegistry{
		db:      db,
		hosts:   make(map[string]*Host),
		runners: make(map[string]*Runner),
		logger:  logger.WithField("component", "host-registry"),
	}
}

// RegisterHost registers a new host
func (hr *HostRegistry) RegisterHost(ctx context.Context, instanceName, zone string, grpcAddress string) (*Host, error) {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	hr.logger.WithFields(logrus.Fields{
		"instance_name": instanceName,
		"zone":          zone,
	}).Info("Registering host")

	var hostID string
	err := hr.db.QueryRowContext(ctx, `
		INSERT INTO hosts (instance_name, zone, grpc_address, status, last_heartbeat)
		VALUES ($1, $2, $3, 'ready', NOW())
		ON CONFLICT (instance_name) DO UPDATE SET
			zone = EXCLUDED.zone,
			grpc_address = EXCLUDED.grpc_address,
			status = 'ready',
			last_heartbeat = NOW()
		RETURNING id
	`, instanceName, zone, grpcAddress).Scan(&hostID)

	if err != nil {
		return nil, fmt.Errorf("failed to register host: %w", err)
	}

	host := &Host{
		ID:            hostID,
		InstanceName:  instanceName,
		Zone:          zone,
		Status:        "ready",
		GRPCAddress:   grpcAddress,
		LastHeartbeat: time.Now(),
		CreatedAt:     time.Now(),
	}

	hr.hosts[hostID] = host

	return host, nil
}

type HostHeartbeat struct {
	InstanceName       string
	Zone               string
	GRPCAddress        string
	HTTPAddress        string
	IdleRunners        int
	BusyRunners        int
	SnapshotVersion    string
	LoadedManifests    map[string]string
	TotalCPUMillicores int
	UsedCPUMillicores  int
	TotalMemoryMB      int
	UsedMemoryMB       int
}

// UpsertHeartbeat upserts the host record and updates heartbeat fields. It preserves
// draining/terminating host states so a draining host doesn't get flipped back to ready
// by a heartbeat.
func (hr *HostRegistry) UpsertHeartbeat(ctx context.Context, hb HostHeartbeat) (*Host, bool, error) {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	if hb.InstanceName == "" {
		return nil, false, fmt.Errorf("missing instance_name")
	}

	var hostID string
	var status string
	err := hr.db.QueryRowContext(ctx, `
		INSERT INTO hosts (
			instance_name, zone, status, idle_runners, busy_runners,
			snapshot_version, grpc_address, http_address, last_heartbeat,
			total_cpu_millicores, used_cpu_millicores, total_memory_mb, used_memory_mb
		)
		VALUES ($1, $2, 'ready', $3, $4, $5, $6, $7, NOW(), $8, $9, $10, $11)
		ON CONFLICT (instance_name) DO UPDATE SET
			zone = EXCLUDED.zone,
			idle_runners = EXCLUDED.idle_runners,
			busy_runners = EXCLUDED.busy_runners,
			snapshot_version = EXCLUDED.snapshot_version,
			grpc_address = EXCLUDED.grpc_address,
			http_address = EXCLUDED.http_address,
			last_heartbeat = NOW(),
			total_cpu_millicores = EXCLUDED.total_cpu_millicores,
			used_cpu_millicores = EXCLUDED.used_cpu_millicores,
			total_memory_mb = EXCLUDED.total_memory_mb,
			used_memory_mb = EXCLUDED.used_memory_mb,
			status = CASE
				WHEN hosts.status IN ('draining','terminating','terminated','unhealthy') THEN hosts.status
				ELSE 'ready'
			END
		RETURNING id, status
	`, hb.InstanceName, hb.Zone, hb.IdleRunners, hb.BusyRunners, hb.SnapshotVersion, hb.GRPCAddress, hb.HTTPAddress,
		hb.TotalCPUMillicores, hb.UsedCPUMillicores, hb.TotalMemoryMB, hb.UsedMemoryMB).Scan(&hostID, &status)
	if err != nil {
		return nil, false, fmt.Errorf("failed to upsert host heartbeat: %w", err)
	}

	host := hr.hosts[hostID]
	if host == nil {
		host = &Host{ID: hostID}
		hr.hosts[hostID] = host
	}

	host.InstanceName = hb.InstanceName
	host.Zone = hb.Zone
	host.Status = status
	host.IdleRunners = hb.IdleRunners
	host.BusyRunners = hb.BusyRunners
	host.SnapshotVersion = hb.SnapshotVersion
	host.GRPCAddress = hb.GRPCAddress
	host.HTTPAddress = hb.HTTPAddress
	host.LastHeartbeat = time.Now()
	host.TotalCPUMillicores = hb.TotalCPUMillicores
	host.UsedCPUMillicores = hb.UsedCPUMillicores
	host.TotalMemoryMB = hb.TotalMemoryMB
	host.UsedMemoryMB = hb.UsedMemoryMB
	if hb.LoadedManifests != nil {
		host.LoadedManifests = hb.LoadedManifests
	}

	return host, status == "draining", nil
}

func (hr *HostRegistry) GetHostByInstanceName(instanceName string) (*Host, bool) {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	for _, h := range hr.hosts {
		if h.InstanceName == instanceName {
			return h, true
		}
	}
	return nil, false
}

func (hr *HostRegistry) SetHostStatusByInstanceName(ctx context.Context, instanceName, status string) error {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	_, err := hr.db.ExecContext(ctx, `UPDATE hosts SET status = $2 WHERE instance_name = $1`, instanceName, status)
	if err != nil {
		return err
	}

	for _, h := range hr.hosts {
		if h.InstanceName == instanceName {
			h.Status = status
			break
		}
	}
	return nil
}

// GetHost returns a host by ID
func (hr *HostRegistry) GetHost(hostID string) (*Host, error) {
	hr.mu.RLock()
	defer hr.mu.RUnlock()

	host, ok := hr.hosts[hostID]
	if !ok {
		return nil, fmt.Errorf("host not found: %s", hostID)
	}
	return host, nil
}

// GetAllHosts returns all hosts
func (hr *HostRegistry) GetAllHosts() []*Host {
	hr.mu.RLock()
	defer hr.mu.RUnlock()

	hosts := make([]*Host, 0, len(hr.hosts))
	for _, h := range hr.hosts {
		hosts = append(hosts, h)
	}
	return hosts
}

// GetAvailableHosts returns hosts that can accept new runners.
// A host is considered available if it has a fresh heartbeat, is ready,
// and has resource capacity remaining.
func (hr *HostRegistry) GetAvailableHosts() []*Host {
	hr.mu.RLock()
	defer hr.mu.RUnlock()

	var available []*Host
	for _, h := range hr.hosts {
		if h.Status != "ready" {
			continue
		}
		if time.Since(h.LastHeartbeat) >= 60*time.Second {
			continue
		}
		if h.TotalCPUMillicores > 0 &&
			(h.TotalCPUMillicores-h.UsedCPUMillicores) > 0 &&
			(h.TotalMemoryMB-h.UsedMemoryMB) > 0 {
			available = append(available, h)
		}
	}
	return available
}

// AddRunner adds or updates a runner in the registry.
// Uses upsert so that session-resumed runners (which reuse the same ID) don't
// fail on duplicate key.
func (hr *HostRegistry) AddRunner(ctx context.Context, runner *Runner) error {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	_, err := hr.db.ExecContext(ctx, `
		INSERT INTO runners (id, host_id, status, internal_ip, job_id, workload_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			host_id = EXCLUDED.host_id,
			status = EXCLUDED.status,
			internal_ip = EXCLUDED.internal_ip
	`, runner.ID, runner.HostID, runner.Status, runner.InternalIP, runner.JobID, runner.WorkloadKey)

	if err != nil {
		return err
	}

	hr.runners[runner.ID] = runner
	return nil
}

// GetRunner returns a runner by ID
func (hr *HostRegistry) GetRunner(runnerID string) (*Runner, error) {
	hr.mu.RLock()
	defer hr.mu.RUnlock()

	runner, ok := hr.runners[runnerID]
	if !ok {
		return nil, fmt.Errorf("runner not found: %s", runnerID)
	}
	return runner, nil
}

// RemoveRunner removes a runner from the registry
func (hr *HostRegistry) RemoveRunner(runnerID string) error {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	_, err := hr.db.Exec(`DELETE FROM runners WHERE id = $1`, runnerID)
	if err != nil {
		return err
	}

	delete(hr.runners, runnerID)
	return nil
}

// GetRunnersByHostID returns all runners belonging to the given host.
func (hr *HostRegistry) GetRunnersByHostID(hostID string) []*Runner {
	hr.mu.RLock()
	defer hr.mu.RUnlock()

	var runners []*Runner
	for _, r := range hr.runners {
		if r.HostID == hostID {
			runners = append(runners, r)
		}
	}
	return runners
}

// RemoveHost removes a host from the in-memory map only.
func (hr *HostRegistry) RemoveHost(hostID string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	delete(hr.hosts, hostID)
}

// CleanupHostRunners deletes all runners for a host from both DB and in-memory map.
func (hr *HostRegistry) CleanupHostRunners(ctx context.Context, hostID string) error {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	_, err := hr.db.ExecContext(ctx, `DELETE FROM runners WHERE host_id = $1`, hostID)
	if err != nil {
		return fmt.Errorf("failed to delete runners for host %s: %w", hostID, err)
	}

	for id, r := range hr.runners {
		if r.HostID == hostID {
			delete(hr.runners, id)
		}
	}
	return nil
}

// HealthCheckLoop periodically checks host health
func (hr *HostRegistry) HealthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hr.checkHostHealth()
		}
	}
}

func (hr *HostRegistry) checkHostHealth() {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	staleThreshold := 90 * time.Second

	for _, host := range hr.hosts {
		if time.Since(host.LastHeartbeat) > staleThreshold {
			if host.Status == "ready" || host.Status == "draining" {
				hr.logger.WithFields(logrus.Fields{
					"host_id":        host.ID,
					"instance_name":  host.InstanceName,
					"last_heartbeat": host.LastHeartbeat,
				}).Warn("Host heartbeat stale, marking unhealthy")
				host.Status = "unhealthy"

				hr.db.Exec(`UPDATE hosts SET status = 'unhealthy' WHERE id = $1`, host.ID)
			}
		}
	}
}

// LoadFromDB loads hosts and runners from database
func (hr *HostRegistry) LoadFromDB(ctx context.Context) error {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	// Load hosts
	rows, err := hr.db.QueryContext(ctx, `
		SELECT id, instance_name, zone, status, idle_runners, busy_runners,
		       snapshot_version, last_heartbeat, grpc_address, http_address, created_at,
		       total_cpu_millicores, used_cpu_millicores, total_memory_mb, used_memory_mb
		FROM hosts
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var h Host
		var snapshotVersion, grpcAddress, httpAddress sql.NullString
		var lastHeartbeat sql.NullTime

		err := rows.Scan(&h.ID, &h.InstanceName, &h.Zone, &h.Status,
			&h.IdleRunners, &h.BusyRunners,
			&snapshotVersion, &lastHeartbeat, &grpcAddress, &httpAddress, &h.CreatedAt,
			&h.TotalCPUMillicores, &h.UsedCPUMillicores, &h.TotalMemoryMB, &h.UsedMemoryMB)
		if err != nil {
			return err
		}

		if snapshotVersion.Valid {
			h.SnapshotVersion = snapshotVersion.String
		}
		if lastHeartbeat.Valid {
			h.LastHeartbeat = lastHeartbeat.Time
		}
		if grpcAddress.Valid {
			h.GRPCAddress = grpcAddress.String
		}
		if httpAddress.Valid {
			h.HTTPAddress = httpAddress.String
		}

		hr.hosts[h.ID] = &h
	}

	// Load runners
	rows, err = hr.db.QueryContext(ctx, `
		SELECT id, host_id, status, internal_ip, job_id, created_at
		FROM runners
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r Runner
		var internalIP, jobID sql.NullString

		err := rows.Scan(&r.ID, &r.HostID, &r.Status, &internalIP,
			&jobID, &r.CreatedAt)
		if err != nil {
			return err
		}

		if internalIP.Valid {
			r.InternalIP = internalIP.String
		}
		if jobID.Valid {
			r.JobID = jobID.String
		}

		hr.runners[r.ID] = &r
	}

	hr.logger.WithFields(logrus.Fields{
		"hosts":   len(hr.hosts),
		"runners": len(hr.runners),
	}).Info("Loaded state from database")

	return nil
}
