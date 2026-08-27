//go:build goexperiment.jsonv2

package main

import (
	"testing"
	"time"
)

func ev(t time.Time, source string, dist, acc float64, hasPos bool) event {
	return event{ts: t, source: source, distanceM: dist, gpsAccuracy: acc, hasPos: hasPos}
}

func TestAnalyzeClassifiesFailures(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mkt := func(minsAgo int) time.Time { return now.Add(time.Duration(-minsAgo) * time.Minute) }

	events := []event{
		// close failure: auto fired 80m out, manual 5 min later (relocked before arrival)
		ev(mkt(200), "auto", 80, 15, true),
		ev(mkt(195), "manual", 0, 0, false),
		// far failure: auto fired 400m out, manual 3 min later (triggered too early)
		ev(mkt(180), "auto", 400, 20, true),
		ev(mkt(177), "manual", 0, 0, false),
		// clean auto (no manual follows)
		ev(mkt(150), "auto", 70, 12, true),
		// bad-GPS fire: accuracy above gate
		ev(mkt(120), "auto", 90, 300, true),
		// missed fire: manual with no auto in preceding 15 min
		ev(mkt(60), "manual", 0, 0, false),
	}

	a := analyze(events, 14*24*time.Hour, now, 100)
	if a.Autos != 4 {
		t.Errorf("autos = %d, want 4", a.Autos)
	}
	if a.Manuals != 3 {
		t.Errorf("manuals = %d, want 3", a.Manuals)
	}
	if a.RelockFails != 2 {
		t.Errorf("relock fails = %d, want 2", a.RelockFails)
	}
	if a.CloseFails != 1 {
		t.Errorf("close fails = %d, want 1", a.CloseFails)
	}
	if a.FarFails != 1 {
		t.Errorf("far fails = %d, want 1", a.FarFails)
	}
	if a.MissedFires != 1 {
		t.Errorf("missed fires = %d, want 1", a.MissedFires)
	}
	if a.BadGPSFires != 1 {
		t.Errorf("bad-gps fires = %d, want 1", a.BadGPSFires)
	}
}

func TestDecideCloseFailLengthensHold(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 10, CloseFails: 1, RelockFails: 1}
	adj := decide(a, 50, 55, 100, 100, tunerState{LastChange: map[string]time.Time{}}, now)
	got := findLever(adj, "unlock_duration")
	if got == nil {
		t.Fatal("expected unlock_duration adjustment")
	}
	if got.To != 65 {
		t.Errorf("unlock_duration to = %v, want 65", got.To)
	}
}

func TestDecideCleanWindowTrimsHold(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 12, RealArrivals: 12, RelockFails: 0}
	adj := decide(a, 50, 60, 100, 100, tunerState{LastChange: map[string]time.Time{}}, now)
	got := findLever(adj, "unlock_duration")
	if got == nil {
		t.Fatal("expected unlock_duration trim")
	}
	if got.To != 55 { // -step/2 = -5
		t.Errorf("unlock_duration to = %v, want 55", got.To)
	}
}

func TestDecideNoTrimOnBackfillOnly(t *testing.T) {
	now := time.Now()
	// 30 backfilled arrivals, zero real ones: must NOT trim the hold.
	a := analysis{Autos: 30, RealArrivals: 0, RelockFails: 0}
	adj := decide(a, 50, 120, 100, 100, tunerState{LastChange: map[string]time.Time{}}, now)
	if findLever(adj, "unlock_duration") != nil {
		t.Error("expected no trim when all arrivals are backfilled history")
	}
}

func TestDecideFarFailsTightenRadius(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 10, RelockFails: 2, FarFails: 2}
	adj := decide(a, 50, 55, 100, 120, tunerState{LastChange: map[string]time.Time{}}, now)
	got := findLever(adj, "trigger_radius")
	if got == nil {
		t.Fatal("expected trigger_radius adjustment")
	}
	if got.To != 100 { // 120 - 20
		t.Errorf("trigger_radius to = %v, want 100", got.To)
	}
}

func TestDecideMissedFireWidensWhenClean(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 10, MissedFires: 2}
	adj := decide(a, 50, 55, 100, 120, tunerState{LastChange: map[string]time.Time{}}, now)
	got := findLever(adj, "trigger_radius")
	if got == nil {
		t.Fatal("expected trigger_radius widen")
	}
	if got.To != 140 { // widen 120 + 20
		t.Errorf("trigger_radius to = %v, want 140", got.To)
	}
}

func TestDecideFarFailsBeatMissedAndTighten(t *testing.T) {
	now := time.Now()
	// far fails take priority over missed fires now (earlier triggering is the
	// worse annoyance, and widening won't fix GPS-glitch fires).
	a := analysis{Autos: 10, MissedFires: 1, FarFails: 3, RelockFails: 3}
	adj := decide(a, 50, 55, 100, 120, tunerState{LastChange: map[string]time.Time{}}, now)
	got := findLever(adj, "trigger_radius")
	if got == nil {
		t.Fatal("expected trigger_radius adjustment")
	}
	if got.To != 100 { // tighten 120 - 20
		t.Errorf("trigger_radius to = %v, want 100", got.To)
	}
}

func TestDecideNoWidenDuringEarlyFires(t *testing.T) {
	now := time.Now()
	// missed fires present, but so are early (glitch) fires -> don't widen; the
	// veto handles glitches and widening would just cause more early fires.
	a := analysis{Autos: 10, MissedFires: 3, EarlyFires: 5}
	adj := decide(a, 50, 55, 100, 120, tunerState{LastChange: map[string]time.Time{}}, now)
	if findLever(adj, "trigger_radius") != nil {
		t.Error("expected no radius widen while early fires are present")
	}
}

func TestAnalyzeGateLateReclassified(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	sAgo := func(s int) time.Time { return now.Add(time.Duration(-s) * time.Second) }
	// auto fired 300s ago with a 15s 2nd-gate stagger; 2nd gate tapped by hand 1s
	// later (well before its delayed unlock) -> gate_late, not close.
	auto := ev(sAgo(300), "auto", 60, 12, true)
	auto.gateDelayS = 15
	manual := event{ts: sAgo(299), source: "manual_dashboard", door: "15238"}
	a := analyze([]event{auto, manual}, 24*time.Hour, now, 100)
	if a.GateLateFails != 1 {
		t.Errorf("gate_late = %d, want 1", a.GateLateFails)
	}
	if a.CloseFails != 0 {
		t.Errorf("close = %d, want 0 (should not inflate hold pressure)", a.CloseFails)
	}
	if a.RelockFails != 1 {
		t.Errorf("relock = %d, want 1", a.RelockFails)
	}
}

func TestAnalyzeGateTapAfterDelayIsCloseFail(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	sAgo := func(s int) time.Time { return now.Add(time.Duration(-s) * time.Second) }
	// 2nd gate delay is 4s; tapped 20s after the fire -> the gate did open, so a
	// re-tap is a genuine relock/hold issue, classified close (not gate_late).
	auto := ev(sAgo(300), "auto", 60, 12, true)
	auto.gateDelayS = 4
	manual := event{ts: sAgo(280), source: "manual_dashboard", door: "15238"}
	a := analyze([]event{auto, manual}, 24*time.Hour, now, 100)
	if a.GateLateFails != 0 {
		t.Errorf("gate_late = %d, want 0", a.GateLateFails)
	}
	if a.CloseFails != 1 {
		t.Errorf("close = %d, want 1", a.CloseFails)
	}
}

func TestDecideGateLateShortensDelay(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 10, GateLateFails: 3, RelockFails: 3}
	adj := decide(a, 4, 55, 100, 100, tunerState{LastChange: map[string]time.Time{}}, now)
	got := findLever(adj, "gate_delay")
	if got == nil {
		t.Fatal("expected gate_delay adjustment")
	}
	if got.To != 3 { // 4 - step(2) = 2, clamped up to the 3s floor
		t.Errorf("gate_delay to = %v, want 3", got.To)
	}
	// gate-late failures must NOT push the hold lever.
	if findLever(adj, "unlock_duration") != nil {
		t.Error("gate-late failures should not change unlock_duration")
	}
}

func TestAnalyzeCountsEarlyAndVetoed(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	sAgo := func(s int) time.Time { return now.Add(time.Duration(-s) * time.Second) }
	// early fire: companion 90m, fresh iCloud3 says 320m (disagree 250)
	early := withIcloud(ev(sAgo(600), "auto", 90, 12, true), 320, 250, 60)
	vetoed := ev(sAgo(500), "auto_vetoed", 0, 0, false)
	a := analyze([]event{early, vetoed}, 24*time.Hour, now, 100)
	if a.EarlyFires != 1 {
		t.Errorf("early_fires = %d, want 1", a.EarlyFires)
	}
	if a.VetoedFires != 1 {
		t.Errorf("vetoed_fires = %d, want 1", a.VetoedFires)
	}
	if a.Autos != 1 {
		t.Errorf("autos = %d, want 1 (vetoed not counted as an arrival)", a.Autos)
	}
}

func TestDecideBadGPSTightensAccuracyGate(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 10, BadGPSFires: 2, GoodAccP90: 30}
	adj := decide(a, 50, 55, 100, 100, tunerState{LastChange: map[string]time.Time{}}, now)
	got := findLever(adj, "accuracy_gate")
	if got == nil {
		t.Fatal("expected accuracy_gate adjustment")
	}
	// target = p90*1.5 = 45; step-limited from 100 by 15 -> 85
	if got.To != 85 {
		t.Errorf("accuracy_gate to = %v, want 85", got.To)
	}
}

func TestDecideRespectsCooldown(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 10, CloseFails: 1, RelockFails: 1}
	st := tunerState{LastChange: map[string]time.Time{"unlock_duration": now.Add(-1 * time.Hour)}}
	adj := decide(a, 50, 55, 100, 100, st, now)
	if findLever(adj, "unlock_duration") != nil {
		t.Error("expected no unlock_duration change during cooldown")
	}
}

func TestDecideRespectsCaps(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 10, CloseFails: 1, RelockFails: 1}
	// already at max; should not exceed
	adj := decide(a, 50, 120, 100, 100, tunerState{LastChange: map[string]time.Time{}}, now)
	if findLever(adj, "unlock_duration") != nil {
		t.Error("expected no change when already at cap")
	}
}

func TestDecideNoRadiusChangeBelowMinSamples(t *testing.T) {
	now := time.Now()
	a := analysis{Autos: 5, FarFails: 3, RelockFails: 3}
	adj := decide(a, 50, 55, 100, 120, tunerState{LastChange: map[string]time.Time{}}, now)
	if findLever(adj, "trigger_radius") != nil {
		t.Error("expected no radius change below min sample count")
	}
}

// withIcloud attaches a fresh (or stale) iCloud3 second opinion to an auto event.
func withIcloud(e event, icloudDist, disagreeM, ageS float64) event {
	e.hasIcloud = true
	e.icloudDistanceM = icloudDist
	e.srcDisagreeM = disagreeM
	e.icloudAgeS = ageS
	return e
}

func TestAnalyzeIcloudDisagreementReclassifiesFar(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mkt := func(minsAgo int) time.Time { return now.Add(time.Duration(-minsAgo) * time.Minute) }

	// Companion says close (80m) but a fresh iCloud3 fix says far (400m) and the
	// two disagree by 320m. A manual follows -> must be a FAR fail, not close.
	auto := withIcloud(ev(mkt(200), "auto", 80, 12, true), 400, 320, 60)
	events := []event{auto, ev(mkt(197), "manual", 0, 0, false)}

	a := analyze(events, 14*24*time.Hour, now, 100)
	if a.FarFails != 1 {
		t.Errorf("far fails = %d, want 1 (reclassified via iCloud3)", a.FarFails)
	}
	if a.CloseFails != 0 {
		t.Errorf("close fails = %d, want 0", a.CloseFails)
	}
	if a.CrossChecked != 1 || a.Disagreements != 1 {
		t.Errorf("cross_checked=%d disagree=%d, want 1/1", a.CrossChecked, a.Disagreements)
	}
}

func TestAnalyzeIcloudAgreementKeepsCompanion(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mkt := func(minsAgo int) time.Time { return now.Add(time.Duration(-minsAgo) * time.Minute) }

	// Companion 80m, iCloud3 88m, disagree only 10m -> verified; stays a close fail.
	auto := withIcloud(ev(mkt(200), "auto", 80, 12, true), 88, 10, 60)
	events := []event{auto, ev(mkt(197), "manual", 0, 0, false)}

	a := analyze(events, 14*24*time.Hour, now, 100)
	if a.CloseFails != 1 || a.FarFails != 0 {
		t.Errorf("close=%d far=%d, want 1/0", a.CloseFails, a.FarFails)
	}
	if a.CrossChecked != 1 || a.Disagreements != 0 {
		t.Errorf("cross_checked=%d disagree=%d, want 1/0", a.CrossChecked, a.Disagreements)
	}
}

func TestAnalyzeStaleIcloudIgnored(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mkt := func(minsAgo int) time.Time { return now.Add(time.Duration(-minsAgo) * time.Minute) }

	// iCloud3 disagrees but its fix is 20 min old (stale) -> ignored; companion
	// (80m, close) wins and it is not counted as cross-checked.
	auto := withIcloud(ev(mkt(200), "auto", 80, 12, true), 400, 320, 20*60)
	events := []event{auto, ev(mkt(197), "manual", 0, 0, false)}

	a := analyze(events, 14*24*time.Hour, now, 100)
	if a.CloseFails != 1 || a.FarFails != 0 {
		t.Errorf("close=%d far=%d, want 1/0 (stale iCloud3 ignored)", a.CloseFails, a.FarFails)
	}
	if a.CrossChecked != 0 {
		t.Errorf("cross_checked=%d, want 0 (stale fix)", a.CrossChecked)
	}
}

func findLever(adj []adjustment, lever string) *adjustment {
	for i := range adj {
		if adj[i].Lever == lever {
			return &adj[i]
		}
	}
	return nil
}
