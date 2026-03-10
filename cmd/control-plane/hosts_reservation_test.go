package main

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestGetAvailableHostsCountsPendingAndUnreportedReservations(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)

	host := &Host{
		ID:                   "host-1",
		Status:               "ready",
		LastHeartbeat:        time.Now(),
		TotalCPUMillicores:   8000,
		UsedCPUMillicores:    2000,
		TotalMemoryMB:        8192,
		UsedMemoryMB:         1024,
		PendingCPUMillicores: 1000,
		PendingMemoryMB:      512,
		RunnerInfos: []HostRunnerInfo{
			{RunnerID: "reported"},
		},
	}
	hr.hosts[host.ID] = host
	hr.runners["reported"] = &Runner{ID: "reported", HostID: host.ID, ReservedCPU: 2000, ReservedMemoryMB: 1024}
	hr.runners["unreported"] = &Runner{ID: "unreported", HostID: host.ID, ReservedCPU: 5000, ReservedMemoryMB: 1024}

	available := hr.GetAvailableHosts()
	if len(available) != 0 {
		t.Fatalf("expected host to be unavailable once pending+unreported reservations are counted, got %d hosts", len(available))
	}
}

func TestRemoveRunnerOnlyRollsBackReportedReservation(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)

	host := &Host{
		ID:                 "host-1",
		Status:             "ready",
		LastHeartbeat:      time.Now(),
		TotalCPUMillicores: 16000,
		UsedCPUMillicores:  8000,
		TotalMemoryMB:      16384,
		UsedMemoryMB:       4096,
		RunnerInfos: []HostRunnerInfo{
			{RunnerID: "reported"},
		},
	}
	hr.hosts[host.ID] = host
	hr.runners["reported"] = &Runner{ID: "reported", HostID: host.ID, ReservedCPU: 4000, ReservedMemoryMB: 2048}
	hr.runners["unreported"] = &Runner{ID: "unreported", HostID: host.ID, ReservedCPU: 2000, ReservedMemoryMB: 1024}

	if err := hr.RemoveRunner("unreported"); err != nil {
		t.Fatalf("RemoveRunner(unreported) error = %v", err)
	}
	if host.UsedCPUMillicores != 8000 || host.UsedMemoryMB != 4096 {
		t.Fatalf("unreported runner should not change heartbeat-reported usage, got cpu=%d mem=%d", host.UsedCPUMillicores, host.UsedMemoryMB)
	}

	if err := hr.RemoveRunner("reported"); err != nil {
		t.Fatalf("RemoveRunner(reported) error = %v", err)
	}
	if host.UsedCPUMillicores != 4000 || host.UsedMemoryMB != 2048 {
		t.Fatalf("reported runner should roll back host usage, got cpu=%d mem=%d", host.UsedCPUMillicores, host.UsedMemoryMB)
	}
}
