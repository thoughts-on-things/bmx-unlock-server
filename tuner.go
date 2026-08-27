//go:build goexperiment.jsonv2

package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"
)

// The tuner is a feedback controller that adjusts arrival-unlock settings from
// usage data. Every unlock trigger is recorded in events.csv with its source
// (auto = HA arrival automation, manual = iOS shortcut / sequence, or
// manual_dashboard = per-door buttons). A manual unlock shortly after an auto
// fire is the failure signal: it means the automation fired too early and the
// gate had re-locked (or never fired) by the time you arrived.
//
// Failures are split by the distance-from-home at the auto fire:
//   - close failure (< closeFailThresholdM): the trigger was fine but the hold
//     window expired before you reached the gate -> lengthen the hold.
//   - far failure (>= closeFailThresholdM, good GPS): the trigger fired while
//     you were still blocks away -> shrink the trigger radius.
//   - bad-GPS fire (accuracy above the gate): a jittered fix placed you home
//     while you were far -> tighten the GPS accuracy gate.
//
// A manual unlock with no auto fire in the preceding window is a "missed" fire:
// you arrived but the automation never triggered -> the trigger radius is too
// tight, push it back up.

const (
	// closeFailThresholdM separates "hold too short" from "triggered too early".
	closeFailThresholdM = 150.0
	// relockWindow is how long after an auto fire a manual unlock still counts
	// as "the automation failed this arrival".
	relockWindow = 15 * time.Minute
	// icloudFreshWindow is how recent an iCloud3 fix must be (at the moment the
	// auto fire was recorded) to count as a usable second opinion. iCloud3 polls
	// on its own cadence, so a stale fix is ignored rather than trusted.
	icloudFreshWindow = 10 * time.Minute
	// sourceDisagreeThresholdM is how far the companion-app and iCloud3 fixes may
	// differ before the companion fix is treated as unreliable and the iCloud3
	// distance is used for failure classification instead.
	sourceDisagreeThresholdM = 150.0
)

// leverCaps are the hard bounds the tuner will never move a setting past.
type leverCap struct{ min, max, step float64 }

var caps = map[string]leverCap{
	"unlock_duration": {min: 30, max: 120, step: 10},
	"gate_delay":      {min: 3, max: 30, step: 2}, // floor 3s: keep the 2nd gate stagger in the user's preferred 3-5s range
	"accuracy_gate":   {min: 40, max: 120, step: 15},
	"trigger_radius":  {min: 60, max: 200, step: 20},
}

// secondGateDoorID is the ButterflyMX door ID of the 2nd gate. A manual_dashboard
// unlock of this door shortly after an auto fire, but before the 2nd gate's
// staggered (gate_delay) unlock, means the gate opened too late for the arrival.
const secondGateDoorID = "15238"

// leverCooldown is the minimum time between changes to the same lever, so the
// controller observes the effect of a change before making another.
const leverCooldown = 24 * time.Hour

// minSamplesForRadiusAccuracy is how many auto fires must be in the window
// before the tuner will touch the (riskier) radius / accuracy levers.
const minSamplesForRadiusAccuracy = 8

// minCleanArrivalsForTrim is how many REAL (post-instrumentation) arrivals with
// no failures are required before the efficiency trim will shorten the hold.
// Backfilled history (source auto_seed) is excluded: it predates failure
// tracking, so its "zero failures" is an artifact, not evidence.
const minCleanArrivalsForTrim = 8

// event is one row of events.csv.
type event struct {
	ts          time.Time
	source      string
	door        string
	lat, lon    float64
	gpsAccuracy float64
	distanceM   float64
	zoneRadiusM float64
	gateDelayS  float64
	unlockDurS  float64
	accGateM    float64
	trigRadM    float64
	hasPos      bool

	// iCloud3 cross-check (verify-only): an independent iCloud3 fix captured at
	// the same instant as the companion-app fix, used to validate the primary
	// GPS. See analyze() for how disagreement reclassifies a failure.
	icloudLat, icloudLon float64
	icloudAccuracy       float64
	icloudDistanceM      float64 // haversine(iCloud3 fix, home)
	icloudAgeS           float64 // seconds since the iCloud3 fix was located (freshness)
	srcDisagreeM         float64 // haversine(companion fix, iCloud3 fix)
	hasIcloud            bool    // an iCloud3 fix was supplied with this event
}

// eventCSVHeader is the canonical column order of events.csv. ensureEventLogSchema
// migrates older logs (which lack the trailing iCloud3 columns) up to this shape.
var eventCSVHeader = []string{
	"ts", "source", "door", "lat", "lon", "gps_accuracy_m", "distance_from_home_m",
	"ha_zone_radius_m", "gate_delay_s", "unlock_duration_s", "accuracy_gate_m", "trigger_radius_m",
	"icloud_lat", "icloud_lon", "icloud_gps_accuracy_m", "icloud_distance_from_home_m",
	"icloud_age_s", "source_disagreement_m",
}

// tunerState is persisted across restarts so cooldowns survive.
type tunerState struct {
	LastChange   map[string]time.Time `json:"last_change"` // per-lever last adjustment
	LastRun      time.Time            `json:"last_run"`
	LastDecision []adjustment         `json:"last_decision"` // most recent run's changes
	LastSummary  string               `json:"last_summary"`
}

// adjustment is a single proposed/applied change to one lever.
type adjustment struct {
	Lever   string  `json:"lever"`
	Entity  string  `json:"entity"`
	From    float64 `json:"from"`
	To      float64 `json:"to"`
	Reason  string  `json:"reason"`
	Applied bool    `json:"applied"`
}

// analysis is the summary of the trailing window used to make decisions.
type analysis struct {
	Window       time.Duration `json:"window"`
	Autos        int           `json:"auto_fires"`
	RealArrivals int           `json:"real_arrivals"` // auto fires excluding backfilled history
	Manuals      int           `json:"manual_unlocks"`
	RelockFails   int          `json:"relock_failures"`
	CloseFails    int          `json:"close_failures"`
	FarFails      int          `json:"far_failures"`
	GateLateFails int          `json:"gate_late_failures"` // 2nd gate opened by hand before its delayed unlock
	MissedFires   int          `json:"missed_fires"`
	EarlyFires    int          `json:"early_fires"`  // iCloud3 confirms you were far when the auto fired
	VetoedFires   int          `json:"vetoed_fires"` // auto fires the server blocked because iCloud3 said far
	BadGPSFires   int          `json:"bad_gps_fires"`
	GoodAccP90   float64       `json:"good_accuracy_p90"`
	FailureRate  float64       `json:"failure_rate"`

	// iCloud3 cross-check (verify-only) stats over the auto fires in the window.
	CrossChecked  int     `json:"cross_checked"`            // autos that had a fresh iCloud3 second opinion
	Disagreements int     `json:"source_disagreements"`    // of those, how many contradicted the companion fix
	DisagreeRate  float64 `json:"source_disagreement_rate"` // Disagreements / CrossChecked
}

// logEvent appends one row to events.csv, creating it with a header if needed.
// Position fields are left blank when hasPos is false (e.g. manual unlocks).
func (s *server) logEvent(ev event) {
	s.eventLogMu.Lock()
	defer s.eventLogMu.Unlock()

	needHeader := false
	if _, err := os.Stat(s.eventLogPath); os.IsNotExist(err) {
		needHeader = true
	}
	f, err := os.OpenFile(s.eventLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Error("event log: open failed", "err", err)
		return
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if needHeader {
		w.Write(eventCSVHeader)
	}
	blank := func(v float64, has bool, format string) string {
		if !has {
			return ""
		}
		return fmt.Sprintf(format, v)
	}
	w.Write([]string{
		ev.ts.UTC().Format(time.RFC3339),
		ev.source,
		ev.door,
		blank(ev.lat, ev.hasPos, "%.6f"),
		blank(ev.lon, ev.hasPos, "%.6f"),
		blank(ev.gpsAccuracy, ev.hasPos, "%.1f"),
		blank(ev.distanceM, ev.hasPos, "%.1f"),
		blank(ev.zoneRadiusM, ev.hasPos, "%.0f"),
		fmt.Sprintf("%.0f", ev.gateDelayS),
		fmt.Sprintf("%.0f", ev.unlockDurS),
		fmt.Sprintf("%.0f", ev.accGateM),
		fmt.Sprintf("%.0f", ev.trigRadM),
		blank(ev.icloudLat, ev.hasIcloud, "%.6f"),
		blank(ev.icloudLon, ev.hasIcloud, "%.6f"),
		blank(ev.icloudAccuracy, ev.hasIcloud, "%.1f"),
		blank(ev.icloudDistanceM, ev.hasIcloud, "%.1f"),
		blank(ev.icloudAgeS, ev.hasIcloud, "%.0f"),
		blank(ev.srcDisagreeM, ev.hasIcloud && ev.hasPos, "%.1f"),
	})
	w.Flush()
}

// configSnapshot returns the current tunable values under the config lock.
func (s *server) configSnapshot() (gateDelay, unlockDur, accGate, trigRad float64) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.gateDelaySec, s.unlockDurationSec, s.accuracyGateM, s.triggerRadiusM
}

// readEvents loads and parses events.csv, sorted oldest-first.
func (s *server) readEvents() ([]event, error) {
	s.eventLogMu.Lock()
	f, err := os.Open(s.eventLogPath)
	s.eventLogMu.Unlock()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1 // tolerate ragged rows across schema versions
	rows, err := rd.ReadAll()
	if err != nil || len(rows) < 2 {
		return nil, err
	}
	idx := map[string]int{}
	for i, h := range rows[0] {
		idx[h] = i
	}
	get := func(row []string, key string) string {
		if i, ok := idx[key]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	getf := func(row []string, key string) (float64, bool) {
		s := get(row, key)
		if s == "" {
			return 0, false
		}
		v, err := strconv.ParseFloat(s, 64)
		return v, err == nil
	}

	out := make([]event, 0, len(rows)-1)
	for _, row := range rows[1:] {
		ts, err := time.Parse(time.RFC3339, get(row, "ts"))
		if err != nil {
			continue
		}
		ev := event{ts: ts, source: get(row, "source"), door: get(row, "door")}
		lat, okLat := getf(row, "lat")
		lon, okLon := getf(row, "lon")
		acc, _ := getf(row, "gps_accuracy_m")
		dist, okDist := getf(row, "distance_from_home_m")
		ev.lat, ev.lon, ev.gpsAccuracy, ev.distanceM = lat, lon, acc, dist
		ev.hasPos = okLat && okLon && okDist
		ev.zoneRadiusM, _ = getf(row, "ha_zone_radius_m")
		ev.gateDelayS, _ = getf(row, "gate_delay_s")
		ev.unlockDurS, _ = getf(row, "unlock_duration_s")
		ev.accGateM, _ = getf(row, "accuracy_gate_m")
		ev.trigRadM, _ = getf(row, "trigger_radius_m")
		icLat, okIcLat := getf(row, "icloud_lat")
		icLon, okIcLon := getf(row, "icloud_lon")
		ev.icloudLat, ev.icloudLon = icLat, icLon
		ev.icloudAccuracy, _ = getf(row, "icloud_gps_accuracy_m")
		ev.icloudDistanceM, _ = getf(row, "icloud_distance_from_home_m")
		ev.icloudAgeS, _ = getf(row, "icloud_age_s")
		ev.srcDisagreeM, _ = getf(row, "source_disagreement_m")
		ev.hasIcloud = okIcLat && okIcLon
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts.Before(out[j].ts) })
	return out, nil
}

// analyze computes window statistics and failure labels from the event log.
// window bounds how far back to look; only auto/manual events are considered.
func analyze(events []event, window time.Duration, now time.Time, accGate float64) analysis {
	cutoff := now.Add(-window)
	var autos, manuals []event
	vetoed := 0
	for _, ev := range events {
		if ev.ts.Before(cutoff) {
			continue
		}
		switch ev.source {
		case "auto", "geofence", "auto_seed":
			autos = append(autos, ev)
		case "manual", "manual_dashboard":
			manuals = append(manuals, ev)
		case "auto_vetoed":
			vetoed++
		}
	}

	a := analysis{Window: window, Autos: len(autos), Manuals: len(manuals), VetoedFires: vetoed}
	for _, auto := range autos {
		if auto.source != "auto_seed" {
			a.RealArrivals++
		}
	}

	// Match each manual unlock to the most recent auto fire in the relock window.
	for _, m := range manuals {
		var matched *event
		for i := range autos {
			auto := autos[i]
			if auto.ts.After(m.ts) {
				continue
			}
			if m.ts.Sub(auto.ts) <= relockWindow {
				if matched == nil || auto.ts.After(matched.ts) {
					ac := autos[i]
					matched = &ac
				}
			}
		}
		if matched == nil {
			a.MissedFires++ // arrived and unlocked by hand; automation never fired
			continue
		}
		a.RelockFails++
		// A 2nd-gate dashboard unlock that lands BEFORE the 2nd gate's staggered
		// (gate_delay) auto-unlock is not a "hold too short" failure — the gate
		// just opened too late for how fast you reached it. Route it to gate_delay.
		gap := m.ts.Sub(matched.ts).Seconds()
		if m.source == "manual_dashboard" && m.door == secondGateDoorID && gap < matched.gateDelayS {
			a.GateLateFails++
			continue
		}
		// Prefer the verified distance: when a fresh iCloud3 fix contradicts the
		// companion fix, classify by the independent iCloud3 distance instead.
		dist, hasDist := effectiveDistance(*matched)
		if hasDist && dist >= closeFailThresholdM {
			a.FarFails++
		} else {
			a.CloseFails++
		}
	}

	// iCloud3 cross-check tally: how often the two sources agreed vs disagreed
	// among the auto fires that had a fresh iCloud3 second opinion.
	for _, auto := range autos {
		if !crossFresh(auto) {
			continue
		}
		a.CrossChecked++
		if auto.hasPos && auto.srcDisagreeM > sourceDisagreeThresholdM {
			a.Disagreements++
			// Early fire: companion GPS placed you near home but a fresh iCloud3
			// fix says you were still far — the trigger fired too soon.
			if auto.icloudDistanceM > auto.distanceM && auto.icloudDistanceM >= closeFailThresholdM {
				a.EarlyFires++
			}
		}
	}
	if a.CrossChecked > 0 {
		a.DisagreeRate = float64(a.Disagreements) / float64(a.CrossChecked)
	}

	// Bad-GPS fires: auto fires whose reported accuracy is at/above the gate
	// (a jittered fix that slipped through, or history from before the gate).
	var goodAcc []float64
	for _, auto := range autos {
		if !auto.hasPos {
			continue
		}
		if auto.gpsAccuracy >= accGate {
			a.BadGPSFires++
		} else {
			goodAcc = append(goodAcc, auto.gpsAccuracy)
		}
	}
	a.GoodAccP90 = percentile(goodAcc, 0.90)
	if a.Autos > 0 {
		a.FailureRate = float64(a.RelockFails) / float64(a.Autos)
	}
	return a
}

// crossFresh reports whether an auto fire carried an iCloud3 fix recent enough
// to be trusted as an independent second opinion.
func crossFresh(a event) bool {
	return a.hasIcloud && a.icloudAgeS <= icloudFreshWindow.Seconds()
}

// effectiveDistance returns the best distance-from-home to classify a failure by.
// When a fresh iCloud3 fix disagrees with the companion fix, the iCloud3 fix is
// treated as ground truth; otherwise the companion fix is used. The bool is false
// only when neither source gives us a usable position.
func effectiveDistance(a event) (float64, bool) {
	fresh := crossFresh(a)
	switch {
	case a.hasPos && fresh && a.srcDisagreeM > sourceDisagreeThresholdM:
		return a.icloudDistanceM, true // sources disagree: trust the independent iCloud3 fix
	case a.hasPos:
		return a.distanceM, true
	case fresh:
		return a.icloudDistanceM, true
	default:
		return 0, false
	}
}

// decide turns an analysis into at most one adjustment per lever, honoring
// caps, cooldowns and hysteresis. now is used for cooldown checks.
func decide(a analysis, gateDelay, unlockDur, accGate, trigRad float64, st tunerState, now time.Time) []adjustment {
	var adj []adjustment
	coolOK := func(lever string) bool {
		last, ok := st.LastChange[lever]
		return !ok || now.Sub(last) >= leverCooldown
	}
	clamp := func(lever string, v float64) float64 {
		c := caps[lever]
		return math.Max(c.min, math.Min(c.max, v))
	}

	// 1. Hold window (unlock_duration): lengthen on close failures; trim when
	//    the window has been clean, to reduce open time / API load.
	if a.CloseFails > 0 && coolOK("unlock_duration") {
		to := clamp("unlock_duration", unlockDur+caps["unlock_duration"].step)
		if to != unlockDur {
			adj = append(adj, adjustment{
				Lever: "unlock_duration", Entity: "input_number.unlock_duration",
				From: unlockDur, To: to,
				Reason: fmt.Sprintf("%d close re-lock failure(s): gate held too short for arrival", a.CloseFails),
			})
		}
	} else if a.RelockFails == 0 && a.RealArrivals >= minCleanArrivalsForTrim && coolOK("unlock_duration") {
		to := clamp("unlock_duration", unlockDur-caps["unlock_duration"].step/2)
		if to != unlockDur {
			adj = append(adj, adjustment{
				Lever: "unlock_duration", Entity: "input_number.unlock_duration",
				From: unlockDur, To: to,
				Reason: fmt.Sprintf("%d real clean arrivals, no failures: trimming hold to reduce open time", a.RealArrivals),
			})
		}
	}

	// 1b. Gate delay: shorten when the 2nd gate keeps getting opened by hand
	//     before its staggered auto-unlock (you reach it faster than the delay).
	if a.GateLateFails > 0 && coolOK("gate_delay") {
		to := clamp("gate_delay", gateDelay-caps["gate_delay"].step)
		if to != gateDelay {
			adj = append(adj, adjustment{
				Lever: "gate_delay", Entity: "input_number.gate_delay",
				From: gateDelay, To: to,
				Reason: fmt.Sprintf("%d arrival(s) opened the 2nd gate by hand before its %.0fs delay elapsed: shortening", a.GateLateFails, gateDelay),
			})
		}
	}

	// 2. Trigger radius. Far re-lock failures (you unlocked by hand while genuinely
	//    far, iCloud3-corrected) pull it in. Missed arrivals push it out, but not
	//    during glitchy windows (early fires) — GPS-glitch early fires are handled
	//    by the server-side iCloud3 veto, not by moving the radius (a glitch reports
	//    an in-radius distance no matter where the radius sits).
	if a.Autos >= minSamplesForRadiusAccuracy && coolOK("trigger_radius") {
		switch {
		case a.FarFails >= 2:
			to := clamp("trigger_radius", trigRad-caps["trigger_radius"].step)
			if to != trigRad {
				adj = append(adj, adjustment{
					Lever: "trigger_radius", Entity: "input_number.gate_trigger_radius",
					From: trigRad, To: to,
					Reason: fmt.Sprintf("%d far re-lock failure(s): triggering too early, tightening radius", a.FarFails),
				})
			}
		case a.MissedFires > 0 && a.EarlyFires == 0:
			to := clamp("trigger_radius", trigRad+caps["trigger_radius"].step)
			if to != trigRad {
				adj = append(adj, adjustment{
					Lever: "trigger_radius", Entity: "input_number.gate_trigger_radius",
					From: trigRad, To: to,
					Reason: fmt.Sprintf("%d missed arrival(s): radius too tight, automation didn't fire", a.MissedFires),
				})
			}
		}
	}

	// 3. GPS accuracy gate: tighten toward the good-fix p90 (with margin) when
	//    bad-GPS fires slip through.
	if a.BadGPSFires > 0 && a.Autos >= minSamplesForRadiusAccuracy && a.GoodAccP90 > 0 && coolOK("accuracy_gate") {
		target := clamp("accuracy_gate", a.GoodAccP90*1.5)
		if target < accGate {
			to := math.Max(target, accGate-caps["accuracy_gate"].step)
			to = clamp("accuracy_gate", to)
			if to != accGate {
				adj = append(adj, adjustment{
					Lever: "accuracy_gate", Entity: "input_number.gps_accuracy_max",
					From: accGate, To: to,
					Reason: fmt.Sprintf("%d bad-GPS fire(s); good fixes p90=%.0fm, tightening gate", a.BadGPSFires, a.GoodAccP90),
				})
			}
		}
	}

	return adj
}

// runTuner is the background loop. It runs once shortly after startup, then on
// the configured interval.
func (s *server) runTuner(ctx context.Context) {
	timer := time.NewTimer(2 * time.Minute) // first pass after startup settles
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.tuneOnce(ctx)
			timer.Reset(s.tuneInterval)
		}
	}
}

// tuneOnce runs one analyze->decide->apply cycle and persists state.
func (s *server) tuneOnce(ctx context.Context) (analysis, []adjustment) {
	events, err := s.readEvents()
	if err != nil {
		slog.Error("tuner: read events failed", "err", err)
	}
	now := time.Now()
	gateDelay, unlockDur, accGate, trigRad := s.configSnapshot()

	window := s.tuneWindow
	a := analyze(events, window, now, accGate)

	st := s.loadTunerState()
	adj := decide(a, gateDelay, unlockDur, accGate, trigRad, st, now)

	summary := fmt.Sprintf("autos=%d (real=%d) manual=%d relock_fail=%d (close=%d far=%d gate_late=%d) missed=%d early=%d bad_gps=%d fail_rate=%.0f%% icloud_checked=%d disagree=%d",
		a.Autos, a.RealArrivals, a.Manuals, a.RelockFails, a.CloseFails, a.FarFails, a.GateLateFails, a.MissedFires, a.EarlyFires, a.BadGPSFires, a.FailureRate*100, a.CrossChecked, a.Disagreements)

	if len(adj) == 0 {
		slog.Info("tuner: no change", "window", window.String(), "stats", summary)
	}
	applyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for i := range adj {
		applied, err := s.applyAdjustment(applyCtx, adj[i])
		if err != nil {
			slog.Error("tuner: apply failed", "lever", adj[i].Lever, "err", err)
			continue
		}
		adj[i].Applied = applied
		if applied {
			// Only real applications start a cooldown; recommendations repeat
			// each run so they stay visible until a token is added.
			st.LastChange[adj[i].Lever] = now
			slog.Info("tuner: adjusted",
				"lever", adj[i].Lever, "from", adj[i].From, "to", adj[i].To, "reason", adj[i].Reason)
		} else {
			slog.Info("tuner: recommendation (recommend-only, not applied)",
				"lever", adj[i].Lever, "from", adj[i].From, "to", adj[i].To, "reason", adj[i].Reason)
		}
	}

	st.LastRun = now
	st.LastDecision = adj
	st.LastSummary = summary
	s.saveTunerState(st)
	s.appendTuningLog(now, a, adj)
	return a, adj
}

// applyAdjustment writes the change to the HA input_number helper (the source of
// truth) and reflects it locally. Returns applied=false, changing nothing, when
// no HA token is configured (recommend-only mode). HA's config-sync automation
// pushes the new value back to /config, so the local write here is only for
// immediate consistency.
func (s *server) applyAdjustment(ctx context.Context, a adjustment) (applied bool, err error) {
	if s.haToken == "" {
		return false, nil // recommend-only: touch nothing
	}
	if err := s.haSetInputNumber(ctx, a.Entity, a.To); err != nil {
		return false, err
	}
	s.configMu.Lock()
	switch a.Lever {
	case "unlock_duration":
		s.unlockDurationSec = a.To
	case "gate_delay":
		s.gateDelaySec = a.To
	case "accuracy_gate":
		s.accuracyGateM = a.To
	case "trigger_radius":
		s.triggerRadiusM = a.To
	}
	s.configMu.Unlock()
	return true, nil
}

// haSetInputNumber calls the Home Assistant input_number.set_value service.
func (s *server) haSetInputNumber(ctx context.Context, entity string, value float64) error {
	body, _ := json.Marshal(map[string]any{"entity_id": entity, "value": value})
	url := s.haURL + "/api/services/input_number/set_value"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.haToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HA API %s returned %d", entity, resp.StatusCode)
	}
	return nil
}

// --- state + tuning log persistence ---

func (s *server) loadTunerState() tunerState {
	st := tunerState{LastChange: map[string]time.Time{}}
	b, err := os.ReadFile(s.tunerStatePath)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	if st.LastChange == nil {
		st.LastChange = map[string]time.Time{}
	}
	return st
}

func (s *server) saveTunerState(st tunerState) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(s.tunerStatePath, b, 0644); err != nil {
		slog.Error("tuner: save state failed", "err", err)
	}
}

// appendTuningLog writes a human-readable line per run to tuning.log.
func (s *server) appendTuningLog(now time.Time, a analysis, adj []adjustment) {
	f, err := os.OpenFile(s.tuningLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	line := fmt.Sprintf("%s | autos=%d(real=%d) manual=%d relock=%d(close=%d far=%d gate_late=%d) missed=%d early=%d bad_gps=%d icloud_checked=%d disagree=%d",
		now.UTC().Format(time.RFC3339), a.Autos, a.RealArrivals, a.Manuals, a.RelockFails, a.CloseFails, a.FarFails, a.GateLateFails, a.MissedFires, a.EarlyFires, a.BadGPSFires, a.CrossChecked, a.Disagreements)
	if len(adj) == 0 {
		fmt.Fprintf(f, "%s | no change\n", line)
		return
	}
	for _, x := range adj {
		fmt.Fprintf(f, "%s | %s %.0f->%.0f (%s)%s\n", line, x.Lever, x.From, x.To, x.Reason, dryRunTag(x.Applied, s.haToken))
	}
}

func dryRunTag(applied bool, token string) string {
	if token == "" {
		return " [recommend-only]"
	}
	if !applied {
		return " [apply-failed]"
	}
	return ""
}

// handleTuning returns the current tuner state and window analysis.
// GET /tuning
func (s *server) handleTuning(w http.ResponseWriter, r *http.Request) {
	events, _ := s.readEvents()
	_, _, accGate, _ := s.configSnapshot()
	a := analyze(events, s.tuneWindow, time.Now(), accGate)
	st := s.loadTunerState()
	gateDelay, unlockDur, accG, trigRad := s.configSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"current": map[string]float64{
			"gate_delay_seconds":      gateDelay,
			"unlock_duration_seconds": unlockDur,
			"accuracy_gate_m":         accG,
			"trigger_radius_m":        trigRad,
		},
		"window_analysis": a,
		"last_run":        st.LastRun,
		"last_decision":   st.LastDecision,
		"auto_apply":      s.haToken != "",
	})
}

// handleTuneRun triggers an immediate tuning cycle (for testing/manual use).
// POST /tune/run
func (s *server) handleTuneRun(w http.ResponseWriter, r *http.Request) {
	a, adj := s.tuneOnce(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"analysis": a, "adjustments": adj})
}

// ensureEventLogSchema upgrades an existing events.csv written by an older build
// (before the iCloud3 columns) to the current eventCSVHeader, padding old rows
// with blanks. It is idempotent and a no-op when the file is missing or already
// current. Must run at startup before any new rows are appended, so the file
// never mixes header widths.
func (s *server) ensureEventLogSchema() {
	s.eventLogMu.Lock()
	defer s.eventLogMu.Unlock()

	f, err := os.Open(s.eventLogPath)
	if err != nil {
		return // no file yet; logEvent will write the full header on first write
	}
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	f.Close()
	if err != nil || len(rows) == 0 {
		return
	}
	last := eventCSVHeader[len(eventCSVHeader)-1]
	if len(rows[0]) == len(eventCSVHeader) && rows[0][len(rows[0])-1] == last {
		return // already current
	}

	oldIdx := map[string]int{}
	for i, h := range rows[0] {
		oldIdx[h] = i
	}
	tmp := s.eventLogPath + ".migrating"
	out, err := os.Create(tmp)
	if err != nil {
		slog.Error("tuner: schema migration open failed", "err", err)
		return
	}
	w := csv.NewWriter(out)
	w.Write(eventCSVHeader)
	for _, row := range rows[1:] {
		nr := make([]string, len(eventCSVHeader))
		for j, name := range eventCSVHeader {
			if oi, ok := oldIdx[name]; ok && oi < len(row) {
				nr[j] = row[oi]
			}
		}
		w.Write(nr)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		out.Close()
		os.Remove(tmp)
		slog.Error("tuner: schema migration write failed", "err", err)
		return
	}
	out.Close()
	if err := os.Rename(tmp, s.eventLogPath); err != nil {
		slog.Error("tuner: schema migration rename failed", "err", err)
		return
	}
	slog.Info("tuner: migrated events.csv to current schema", "rows", len(rows)-1, "cols", len(eventCSVHeader))
}

// seedEventsFromArrivals performs a one-time backfill of events.csv from the
// legacy arrivals.csv (all rows are auto fires) so the tuner has historical
// distance/accuracy distributions on first run. No-op if events.csv exists.
func (s *server) seedEventsFromArrivals() {
	if _, err := os.Stat(s.eventLogPath); err == nil {
		return // already have events
	}
	f, err := os.Open(s.arrivalLogPath)
	if err != nil {
		return
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) < 2 {
		return
	}
	idx := map[string]int{}
	for i, h := range rows[0] {
		idx[h] = i
	}
	n := 0
	for _, row := range rows[1:] {
		get := func(k string) string {
			if i, ok := idx[k]; ok && i < len(row) {
				return row[i]
			}
			return ""
		}
		ts, err := time.Parse(time.RFC3339, get("fired_at"))
		if err != nil {
			continue
		}
		pf := func(k string) float64 { v, _ := strconv.ParseFloat(get(k), 64); return v }
		s.logEvent(event{
			ts:          ts,
			source:      "auto_seed",
			lat:         pf("lat"),
			lon:         pf("lon"),
			gpsAccuracy: pf("gps_accuracy_m"),
			distanceM:   pf("distance_from_home_m"),
			zoneRadiusM: pf("ha_zone_radius_m"),
			hasPos:      true,
		})
		n++
	}
	if n > 0 {
		slog.Info("tuner: seeded events.csv from arrivals.csv", "rows", n)
	}
}

// percentile returns the p-quantile (0..1) of vs using linear interpolation.
func percentile(vs []float64, p float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	if len(s) == 1 {
		return s[0]
	}
	k := float64(len(s)-1) * p
	f := int(math.Floor(k))
	if f+1 >= len(s) {
		return s[len(s)-1]
	}
	return s[f] + (s[f+1]-s[f])*(k-float64(f))
}
