package main

import (
	"testing"
	"time"
)

func TestScoreHost_PrefersIdleRunners(t *testing.T) {
	s := &Scheduler{}

	hostA := &Host{TotalSlots: 10, UsedSlots: 5, IdleRunners: 3, LastHeartbeat: time.Now()}
	hostB := &Host{TotalSlots: 10, UsedSlots: 5, IdleRunners: 0, LastHeartbeat: time.Now()}

	scoreA := s.scoreHost(hostA)
	scoreB := s.scoreHost(hostB)

	if scoreA <= scoreB {
		t.Errorf("Host with idle runners should score higher: A=%f, B=%f", scoreA, scoreB)
	}
}

func TestScoreHost_PrefersAvailableCapacity(t *testing.T) {
	s := &Scheduler{}

	hostA := &Host{TotalSlots: 10, UsedSlots: 2, LastHeartbeat: time.Now()}
	hostB := &Host{TotalSlots: 10, UsedSlots: 8, LastHeartbeat: time.Now()}

	scoreA := s.scoreHost(hostA)
	scoreB := s.scoreHost(hostB)

	if scoreA <= scoreB {
		t.Errorf("Host with more capacity should score higher: A=%f, B=%f", scoreA, scoreB)
	}
}

func TestScoreHost_PenalizesHighUtilization(t *testing.T) {
	s := &Scheduler{}

	hostNormal := &Host{TotalSlots: 10, UsedSlots: 5, LastHeartbeat: time.Now()}
	hostHigh := &Host{TotalSlots: 10, UsedSlots: 9, LastHeartbeat: time.Now()}

	scoreNormal := s.scoreHost(hostNormal)
	scoreHigh := s.scoreHost(hostHigh)

	if scoreNormal <= scoreHigh {
		t.Errorf("High utilization host should score lower: normal=%f, high=%f", scoreNormal, scoreHigh)
	}
}

func TestScoreHost_PrefersRecentHeartbeat(t *testing.T) {
	s := &Scheduler{}

	hostRecent := &Host{TotalSlots: 10, UsedSlots: 5, LastHeartbeat: time.Now()}
	hostStale := &Host{TotalSlots: 10, UsedSlots: 5, LastHeartbeat: time.Now().Add(-2 * time.Minute)}

	scoreRecent := s.scoreHost(hostRecent)
	scoreStale := s.scoreHost(hostStale)

	if scoreRecent <= scoreStale {
		t.Errorf("Recent heartbeat should score higher: recent=%f, stale=%f", scoreRecent, scoreStale)
	}
}

func TestScoreHostForRepo_WarmCacheAffinity(t *testing.T) {
	s := &Scheduler{}

	hostWarm := &Host{
		TotalSlots:      10,
		UsedSlots:       5,
		LastHeartbeat:   time.Now(),
		LoadedManifests: map[string]string{"org-repo": "v1"},
	}
	hostCold := &Host{
		TotalSlots:    10,
		UsedSlots:     5,
		LastHeartbeat: time.Now(),
	}

	scoreWarm := s.scoreHostForRepo(hostWarm, "org-repo")
	scoreCold := s.scoreHostForRepo(hostCold, "org-repo")

	if scoreWarm <= scoreCold {
		t.Errorf("Host with warm cache should score higher: warm=%f, cold=%f", scoreWarm, scoreCold)
	}
}

func TestScoreHostForRepo_NoRepoSlug(t *testing.T) {
	s := &Scheduler{}

	host := &Host{
		TotalSlots:      10,
		UsedSlots:       5,
		LastHeartbeat:   time.Now(),
		LoadedManifests: map[string]string{"org-repo": "v1"},
	}

	scoreWithRepo := s.scoreHostForRepo(host, "org-repo")
	scoreNoRepo := s.scoreHostForRepo(host, "")

	// With empty repo slug, no cache affinity bonus should be applied
	if scoreNoRepo >= scoreWithRepo {
		t.Errorf("Empty repo slug should not get cache bonus: with=%f, without=%f", scoreWithRepo, scoreNoRepo)
	}
}

func TestSelectBestHostForRepo(t *testing.T) {
	s := &Scheduler{}

	hosts := []*Host{
		{ID: "cold", TotalSlots: 10, UsedSlots: 5, LastHeartbeat: time.Now()},
		{ID: "warm", TotalSlots: 10, UsedSlots: 5, LastHeartbeat: time.Now(), LoadedManifests: map[string]string{"org-repo": "v1"}},
	}

	best := s.selectBestHostForRepo(hosts, "org-repo")
	if best == nil {
		t.Fatal("selectBestHostForRepo returned nil")
	}
	if best.ID != "warm" {
		t.Errorf("Expected warm host to be selected, got %s", best.ID)
	}
}

func TestSelectBestHostForRepo_Empty(t *testing.T) {
	s := &Scheduler{}
	best := s.selectBestHostForRepo(nil, "org-repo")
	if best != nil {
		t.Error("Expected nil for empty host list")
	}
}
