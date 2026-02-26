package main

import (
	"testing"
	"time"
)

func TestScoreHost_PrefersIdleRunners(t *testing.T) {
	s := &Scheduler{}

	hostA := &Host{TotalCPUMillicores: 16000, UsedCPUMillicores: 8000, TotalMemoryMB: 65536, UsedMemoryMB: 32768, IdleRunners: 3, LastHeartbeat: time.Now()}
	hostB := &Host{TotalCPUMillicores: 16000, UsedCPUMillicores: 8000, TotalMemoryMB: 65536, UsedMemoryMB: 32768, IdleRunners: 0, LastHeartbeat: time.Now()}

	scoreA := s.scoreHost(hostA)
	scoreB := s.scoreHost(hostB)

	if scoreA <= scoreB {
		t.Errorf("Host with idle runners should score higher: A=%f, B=%f", scoreA, scoreB)
	}
}

func TestScoreHost_PrefersAvailableCapacity(t *testing.T) {
	s := &Scheduler{}

	hostA := &Host{TotalCPUMillicores: 16000, UsedCPUMillicores: 3200, TotalMemoryMB: 65536, UsedMemoryMB: 8192, LastHeartbeat: time.Now()}
	hostB := &Host{TotalCPUMillicores: 16000, UsedCPUMillicores: 12800, TotalMemoryMB: 65536, UsedMemoryMB: 52428, LastHeartbeat: time.Now()}

	scoreA := s.scoreHost(hostA)
	scoreB := s.scoreHost(hostB)

	if scoreA <= scoreB {
		t.Errorf("Host with more capacity should score higher: A=%f, B=%f", scoreA, scoreB)
	}
}

func TestScoreHost_PrefersRecentHeartbeat(t *testing.T) {
	s := &Scheduler{}

	hostRecent := &Host{TotalCPUMillicores: 16000, UsedCPUMillicores: 8000, TotalMemoryMB: 65536, UsedMemoryMB: 32768, LastHeartbeat: time.Now()}
	hostStale := &Host{TotalCPUMillicores: 16000, UsedCPUMillicores: 8000, TotalMemoryMB: 65536, UsedMemoryMB: 32768, LastHeartbeat: time.Now().Add(-2 * time.Minute)}

	scoreRecent := s.scoreHost(hostRecent)
	scoreStale := s.scoreHost(hostStale)

	if scoreRecent <= scoreStale {
		t.Errorf("Recent heartbeat should score higher: recent=%f, stale=%f", scoreRecent, scoreStale)
	}
}

func TestScoreHostForWorkloadKey_WarmCacheAffinity(t *testing.T) {
	s := &Scheduler{}

	hostWarm := &Host{
		TotalCPUMillicores: 16000,
		UsedCPUMillicores:  8000,
		TotalMemoryMB:      65536,
		UsedMemoryMB:       32768,
		LastHeartbeat:      time.Now(),
		LoadedManifests:    map[string]string{"org-repo": "v1"},
	}
	hostCold := &Host{
		TotalCPUMillicores: 16000,
		UsedCPUMillicores:  8000,
		TotalMemoryMB:      65536,
		UsedMemoryMB:       32768,
		LastHeartbeat:      time.Now(),
	}

	scoreWarm := s.scoreHostForWorkloadKey(hostWarm, "org-repo")
	scoreCold := s.scoreHostForWorkloadKey(hostCold, "org-repo")

	if scoreWarm <= scoreCold {
		t.Errorf("Host with warm cache should score higher: warm=%f, cold=%f", scoreWarm, scoreCold)
	}
}

func TestScoreHostForWorkloadKey_Empty(t *testing.T) {
	s := &Scheduler{}

	host := &Host{
		TotalCPUMillicores: 16000,
		UsedCPUMillicores:  8000,
		TotalMemoryMB:      65536,
		UsedMemoryMB:       32768,
		LastHeartbeat:      time.Now(),
		LoadedManifests:    map[string]string{"org-repo": "v1"},
	}

	scoreWithRepo := s.scoreHostForWorkloadKey(host, "org-repo")
	scoreNoRepo := s.scoreHostForWorkloadKey(host, "")

	// With empty workload key, no cache affinity bonus should be applied
	if scoreNoRepo >= scoreWithRepo {
		t.Errorf("Empty repo slug should not get cache bonus: with=%f, without=%f", scoreWithRepo, scoreNoRepo)
	}
}

func TestSelectBestHostForRepo(t *testing.T) {
	s := &Scheduler{}

	hosts := []*Host{
		{ID: "cold", TotalCPUMillicores: 16000, UsedCPUMillicores: 8000, TotalMemoryMB: 65536, UsedMemoryMB: 32768, LastHeartbeat: time.Now()},
		{ID: "warm", TotalCPUMillicores: 16000, UsedCPUMillicores: 8000, TotalMemoryMB: 65536, UsedMemoryMB: 32768, LastHeartbeat: time.Now(), LoadedManifests: map[string]string{"org-repo": "v1"}},
	}

	best := s.selectBestHostForWorkloadKey(hosts, "org-repo")
	if best == nil {
		t.Fatal("selectBestHostForRepo returned nil")
	}
	if best.ID != "warm" {
		t.Errorf("Expected warm host to be selected, got %s", best.ID)
	}
}

func TestSelectBestHostForRepo_Empty(t *testing.T) {
	s := &Scheduler{}
	best := s.selectBestHostForWorkloadKey(nil, "org-repo")
	if best != nil {
		t.Error("Expected nil for empty host list")
	}
}
