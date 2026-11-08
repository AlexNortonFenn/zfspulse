package zfspulse

import (
	"context"
	"testing"
)

func TestPoolReliabilityScoring(t *testing.T) {
	// Case 1: Healthy pool with recent scrub (24h drift, threshold 168h -> score 100.0)
	hClean := PoolHealth{
		State:           "ONLINE",
		ScrubDriftHours: 24.0,
	}
	scoreClean := calculateReliabilityScore(&hClean, 168.0)
	if scoreClean != 100.0 {
		t.Errorf("Expected 100%% score for healthy pool, got %.1f", scoreClean)
	}

	// Case 2: Scrub overdue by 14 days (336 hours, 168 hours excess -> penalty (168/24)*5 = 35 -> score 65.0)
	hOverdue := PoolHealth{
		State:           "ONLINE",
		ScrubDriftHours: 336.0,
	}
	scoreOverdue := calculateReliabilityScore(&hOverdue, 168.0)
	if scoreOverdue >= 100.0 || scoreOverdue != 65.0 {
		t.Errorf("Expected score 65.0 for overdue scrub, got %.1f", scoreOverdue)
	}

	// Case 3: Errors present (1 read error (-10), 2 cksum errors (-50) -> score 40.0)
	hErrors := PoolHealth{
		State:          "ONLINE",
		ReadErrors:     1,
		ChecksumErrors: 2,
	}
	scoreErrors := calculateReliabilityScore(&hErrors, 168.0)
	if scoreErrors != 40.0 {
		t.Errorf("Expected score 40.0, got %.1f", scoreErrors)
	}
}

func TestPoolHealthRaiseState(t *testing.T) {
	h := PoolHealth{State: "UNKNOWN"}

	// ONLINE raises UNKNOWN
	h.RaiseState("ONLINE")
	if h.State != "ONLINE" {
		t.Errorf("Expected ONLINE, got %s", h.State)
	}

	// DEGRADED raises ONLINE
	h.RaiseState("DEGRADED")
	if h.State != "DEGRADED" {
		t.Errorf("Expected DEGRADED, got %s", h.State)
	}

	// ONLINE cannot downgrade DEGRADED
	h.RaiseState("ONLINE")
	if h.State != "DEGRADED" {
		t.Errorf("Expected state to remain DEGRADED, got %s", h.State)
	}

	// REMOVED (severity 5) raises DEGRADED (severity 2)
	h.RaiseState("REMOVED")
	if h.State != "REMOVED" {
		t.Errorf("Expected REMOVED to raise DEGRADED, got %s", h.State)
	}

	// REMOVED from UNKNOWN raises to REMOVED
	hFresh := PoolHealth{State: "UNKNOWN"}
	hFresh.RaiseState("REMOVED")
	if hFresh.State != "REMOVED" {
		t.Errorf("Expected REMOVED to raise UNKNOWN, got %s", hFresh.State)
	}

	// REMOVED from ONLINE raises to REMOVED
	hOnline := PoolHealth{State: "ONLINE"}
	hOnline.RaiseState("REMOVED")
	if hOnline.State != "REMOVED" {
		t.Errorf("Expected REMOVED to raise ONLINE, got %s", hOnline.State)
	}

	// FAULTED (severity 5) raises DEGRADED
	hFaulted := PoolHealth{State: "DEGRADED"}
	hFaulted.RaiseState("FAULTED")
	if hFaulted.State != "FAULTED" {
		t.Errorf("Expected FAULTED, got %s", hFaulted.State)
	}

	// Lower severity cannot downgrade FAULTED
	hFaulted.RaiseState("DEGRADED")
	if hFaulted.State != "FAULTED" {
		t.Errorf("Expected state to remain FAULTED, got %s", hFaulted.State)
	}
}

func TestUnknownStateScoresZero(t *testing.T) {
	ins := New(WithProcZfsPath(t.TempDir()))
	ins.cmdRunner = func(ctx context.Context, pool string) zpoolCmdResult {
		return zpoolCmdResult{Stdout: "", Err: nil}
	}

	h, _ := ins.InspectPool(context.Background(), "tank")
	if h.State != "UNKNOWN" {
		t.Fatalf("Expected pool state to be UNKNOWN, got %s", h.State)
	}
	if h.ReliabilityScore != 0.0 {
		t.Errorf("Fail-Closed check failed: UNKNOWN pool must have ReliabilityScore=0.0, got %.1f", h.ReliabilityScore)
	}
}

func TestRemovedStateScoresZero(t *testing.T) {
	hRemoved := PoolHealth{
		State:           "REMOVED",
		ScrubDriftHours: 0.0,
	}
	score := calculateReliabilityScore(&hRemoved, 168.0)
	if score != 0.0 {
		t.Errorf("Expected score 0.0 for REMOVED state, got %.1f", score)
	}
}

func TestScrubInProgressPreservesScore(t *testing.T) {
	hScrubbing := PoolHealth{
		State:           "ONLINE",
		ScrubDriftHours: 9999.0,
		ScrubInProgress: true,
	}
	score := calculateReliabilityScore(&hScrubbing, 168.0)
	if score != 100.0 {
		t.Errorf("Active scrub must not penalize score, expected 100.0, got %.1f", score)
	}
}

func TestRaiseStateUnknownResetsOnline(t *testing.T) {
	// ONLINE must be downgraded to UNKNOWN if state verification fails
	hOnline := PoolHealth{State: "ONLINE"}
	hOnline.RaiseState("UNKNOWN")
	if hOnline.State != "UNKNOWN" {
		t.Errorf("Expected UNKNOWN to reset ONLINE, got %s", hOnline.State)
	}

	// DEGRADED, FAULTED, SUSPENDED must NOT be masked by UNKNOWN
	hDegraded := PoolHealth{State: "DEGRADED"}
	hDegraded.RaiseState("UNKNOWN")
	if hDegraded.State != "DEGRADED" {
		t.Errorf("Expected DEGRADED to be preserved, got %s", hDegraded.State)
	}

	hFaulted := PoolHealth{State: "FAULTED"}
	hFaulted.RaiseState("UNKNOWN")
	if hFaulted.State != "FAULTED" {
		t.Errorf("Expected FAULTED to be preserved, got %s", hFaulted.State)
	}

	hSuspended := PoolHealth{State: "SUSPENDED"}
	hSuspended.RaiseState("UNKNOWN")
	if hSuspended.State != "SUSPENDED" {
		t.Errorf("Expected SUSPENDED to be preserved, got %s", hSuspended.State)
	}
}

func TestSuspendedStateBehavior(t *testing.T) {
	h := PoolHealth{State: "ONLINE"}
	h.RaiseState("SUSPENDED")
	if h.State != "SUSPENDED" {
		t.Errorf("Expected SUSPENDED to raise ONLINE, got %s", h.State)
	}

	score := calculateReliabilityScore(&h, 168.0)
	if score != 0.0 {
		t.Errorf("SUSPENDED pool must have score 0.0, got %.1f", score)
	}

	if h.Severity() != SeverityCritical {
		t.Errorf("SUSPENDED pool must have SeverityCritical, got %s", h.Severity())
	}
}
