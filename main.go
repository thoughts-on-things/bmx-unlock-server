//go:build goexperiment.jsonv2

package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	butterflymx "libdb.so/go-butterflymx"
)

// Unlock sequence: each step fires after the previous one's delay elapses.
// Delay is relative to the previous step (not to sequence start).
type unlockStep struct {
	doorID         butterflymx.ID
	name           string
	delay          time.Duration
	reLockInterval time.Duration // how often to re-unlock during hold; must be < hardware lock duration
	holdDuration   time.Duration // how long to hold this door open; 0 means use global unlockDuration
}

// approachState tracks geofence and in-flight unlock state.
type approachState struct {
	mu             sync.Mutex
	lastUnlockedAt time.Time
	wasOutside     bool   // true when last known position was outside geofence
	cancelSeq      func() // cancels in-flight delayed unlock sequence, nil if none
}

type server struct {
	client            *butterflymx.APIClient
	tenantID          butterflymx.ID
	tenantTagID       butterflymx.TaggedID
	apiKey            string
	homeLat           float64
	homeLon           float64
	radiusM           float64
	cooldown          time.Duration
	sequence          []unlockStep
	approach          approachState
	configMu          sync.RWMutex
	gateDelaySec      float64
	unlockDurationSec float64
	accuracyGateM     float64 // GPS accuracy gate (enforced in HA); tracked for tuning + logging
	triggerRadiusM    float64 // arrival trigger radius (enforced in HA); tracked for tuning + logging
	arrivalLogMu      sync.Mutex
	arrivalLogPath    string

	// event log + auto-tuner
	eventLogMu     sync.Mutex
	eventLogPath   string
	tunerStatePath string
	tuningLogPath  string
	tuneInterval   time.Duration
	tuneWindow     time.Duration
	haURL          string
	haToken        string // long-lived HA token; empty => tuner runs recommend-only
}

// OwnTracks HTTP-mode location payload (subset of fields we need).
type owntracksPayload struct {
	Type string  `json:"_type"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

func main() {
	if err := godotenv.Load(); err != nil {
		slog.Warn("no .env file found, using system env")
	}

	apiToken := mustEnv("BUTTERFLYMX_API_TOKEN")
	apiKey := mustEnv("API_KEY")

	homeLat := mustFloat("HOME_LAT")
	homeLon := mustFloat("HOME_LON")

	radiusM := floatEnvOr("GEOFENCE_RADIUS_M", 200)
	cooldownMin := floatEnvOr("COOLDOWN_MINUTES", 10)
	gateDelaySec := floatEnvOr("GATE_DELAY_SECONDS", 30)
	unlockDurationSec := floatEnvOr("UNLOCK_DURATION_SECONDS", 30)
	accuracyGateM := floatEnvOr("ACCURACY_GATE_M", 100)
	triggerRadiusM := floatEnvOr("TRIGGER_RADIUS_M", 100)

	tuneIntervalH := floatEnvOr("TUNE_INTERVAL_HOURS", 6)
	tuneWindowDays := floatEnvOr("TUNE_WINDOW_DAYS", 14)
	haURL := os.Getenv("HA_URL")
	if haURL == "" {
		haURL = "http://localhost:8123"
	}
	haToken := os.Getenv("HA_TOKEN")

	sequence, err := parseDoors(mustEnv("DOORS"))
	if err != nil {
		slog.Error("invalid DOORS", "err", err)
		os.Exit(1)
	}

	client := butterflymx.NewAPIClient(butterflymx.APIStaticToken(apiToken), nil)

	ctx := context.Background()
	var tenantID butterflymx.ID
	var tenantTagID butterflymx.TaggedID
	for {
		var err error
		tenantID, tenantTagID, err = resolveTenant(ctx, client)
		if err == nil {
			break
		}
		slog.Warn("failed to resolve tenant, retrying in 30s", "err", err)
		time.Sleep(30 * time.Second)
	}
	slog.Info("tenant resolved", "id", tenantID)

	arrivalLogPath := "arrivals.csv"
	if p := os.Getenv("ARRIVAL_LOG_PATH"); p != "" {
		arrivalLogPath = p
	}
	eventLogPath := envOr("EVENT_LOG_PATH", "events.csv")

	s := &server{
		client:            client,
		tenantID:          tenantID,
		tenantTagID:       tenantTagID,
		apiKey:            apiKey,
		homeLat:           homeLat,
		homeLon:           homeLon,
		radiusM:           radiusM,
		cooldown:          time.Duration(cooldownMin * float64(time.Minute)),
		gateDelaySec:      gateDelaySec,
		unlockDurationSec: unlockDurationSec,
		accuracyGateM:     accuracyGateM,
		triggerRadiusM:    triggerRadiusM,
		arrivalLogPath:    arrivalLogPath,
		eventLogPath:      eventLogPath,
		tunerStatePath:    envOr("TUNER_STATE_PATH", "tuner_state.json"),
		tuningLogPath:     envOr("TUNING_LOG_PATH", "tuning.log"),
		tuneInterval:      time.Duration(tuneIntervalH * float64(time.Hour)),
		tuneWindow:        time.Duration(tuneWindowDays * float64(24*time.Hour)),
		haURL:             haURL,
		haToken:           haToken,
		sequence:          sequence,
		approach:          approachState{wasOutside: true},
	}

	// Upgrade any pre-iCloud3 events.csv to the current schema before writing.
	s.ensureEventLogSchema()

	// One-time backfill so the tuner has historical distance/accuracy stats.
	s.seedEventsFromArrivals()

	// Start the auto-tuner in the background.
	tunerCtx, cancelTuner := context.WithCancel(context.Background())
	defer cancelTuner()
	go s.runTuner(tunerCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /doors", s.auth(s.handleDoors))
	mux.HandleFunc("GET /status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /arrivals", s.auth(s.handleArrivalsGet))
	mux.HandleFunc("GET /events", s.auth(s.handleEventsGet))
	mux.HandleFunc("GET /tuning", s.auth(s.handleTuning))
	mux.HandleFunc("POST /tune/run", s.auth(s.handleTuneRun))
	mux.HandleFunc("POST /unlock/sequence", s.auth(s.handleUnlockSequence))
	mux.HandleFunc("POST /unlock/{doors}", s.auth(s.handleUnlock))
	mux.HandleFunc("POST /location", s.auth(s.handleLocation))
	mux.HandleFunc("POST /config", s.auth(s.handleConfig))
	mux.HandleFunc("POST /arrivals/log", s.auth(s.handleArrivalLog))

	addr := ":8080"
	slog.Info("server started",
		"addr", addr,
		"home_lat", homeLat,
		"home_lon", homeLon,
		"radius_m", radiusM,
		"gate_delay_s", gateDelaySec,
		"unlock_duration_s", unlockDurationSec,
		"accuracy_gate_m", accuracyGateM,
		"trigger_radius_m", triggerRadiusM,
		"cooldown_min", cooldownMin,
		"tune_interval", s.tuneInterval.String(),
		"tune_window", s.tuneWindow.String(),
		"auto_apply", haToken != "",
	)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// handleDoors returns the configured door list.
func (s *server) handleDoors(w http.ResponseWriter, r *http.Request) {
	doors := make([]map[string]any, len(s.sequence))
	for i, step := range s.sequence {
		doors[i] = map[string]any{"id": strconv.Itoa(int(step.doorID)), "name": step.name, "online": true}
	}
	writeJSON(w, http.StatusOK, map[string]any{"doors": doors})
}

// secondGateID returns the door ID of the second step in the sequence (the one
// staggered by gate_delay), or "" when only one door is configured.
func (s *server) secondGateID() string {
	if len(s.sequence) < 2 {
		return ""
	}
	return strconv.Itoa(int(s.sequence[1].doorID))
}

// handleStatus fetches live door status from the ButterflyMX API.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	aps, err := butterflymx.CollectResults(s.client.TenantAccessPoints(r.Context(), s.tenantTagID))
	if err != nil {
		slog.Error("failed to fetch access points", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch door status"})
		return
	}

	type doorStatus struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		Online       bool   `json:"online"`
		OpenDuration int    `json:"open_duration_sec"`
	}

	doors := make([]doorStatus, 0, len(aps))
	for _, ap := range aps {
		doors = append(doors, doorStatus{
			ID:           int(ap.ID.Number),
			Name:         ap.Name,
			Online:       ap.Online,
			OpenDuration: ap.OpenDuration,
		})
	}

	s.approach.mu.Lock()
	seqActive := s.approach.cancelSeq != nil
	lastUnlocked := s.approach.lastUnlockedAt
	wasOutside := s.approach.wasOutside
	s.approach.mu.Unlock()

	var cooldownRemaining string
	if !lastUnlocked.IsZero() && time.Since(lastUnlocked) < s.cooldown {
		cooldownRemaining = (s.cooldown - time.Since(lastUnlocked)).Round(time.Second).String()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"doors": doors,
		"geofence": map[string]any{
			"sequence_active":    seqActive,
			"last_unlocked_at":   lastUnlocked,
			"cooldown_remaining": cooldownRemaining,
			"position":           map[string]bool{"outside": wasOutside},
		},
	})
}

// handleUnlock unlocks one or more comma-separated door IDs immediately.
// POST /unlock/1001
// POST /unlock/1001,1002
func (s *server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("doors")
	parts := strings.Split(raw, ",")

	source := sourceOr(r, "manual_dashboard")
	gateDelay, unlockDur, accGate, trigRad := s.configSnapshot()
	s.logEvent(event{
		ts: time.Now(), source: source, door: raw,
		gateDelayS: gateDelay, unlockDurS: unlockDur, accGateM: accGate, trigRadM: trigRad,
	})

	type result struct {
		DoorID string `json:"door_id"`
		Status string `json:"status"`
	}

	results := make([]result, 0, len(parts))
	allOK := true

	for _, part := range parts {
		part = strings.TrimSpace(part)
		id, err := strconv.Atoi(part)
		if err != nil {
			results = append(results, result{DoorID: part, Status: "invalid_id"})
			allOK = false
			continue
		}
		doorID := butterflymx.ID(id)
		if err := s.unlock(r.Context(), doorID, ""); err != nil {
			slog.Error("unlock failed", "door", doorID, "err", err)
			results = append(results, result{DoorID: part, Status: "failed"})
			allOK = false
		} else {
			results = append(results, result{DoorID: part, Status: "unlocked"})
		}
	}

	status := http.StatusOK
	if !allOK {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{"results": results, "all_success": allOK})
}

// handleUnlockSequence triggers the configured approach sequence.
// POST /unlock/sequence?source=auto     (HA arrival automation; optional JSON
//
//	body {lat,lon,gps_accuracy,ha_zone_radius})
//
// POST /unlock/sequence?source=manual   (iOS shortcut / manual sequence)
// The source is recorded in events.csv; a manual unlock shortly after an auto
// fire is the tuner's failure signal.
func (s *server) handleUnlockSequence(w http.ResponseWriter, r *http.Request) {
	source := sourceOr(r, "manual")

	// Optional position payload (sent by the auto path so the tuner can
	// classify failures by distance-from-home). The auto path also includes an
	// independent iCloud3 fix (icloud_*) as a second opinion for verification.
	var body struct {
		Lat               *float64 `json:"lat"`
		Lon               *float64 `json:"lon"`
		GPSAccuracy       float64  `json:"gps_accuracy"`
		HAZoneRadius      float64  `json:"ha_zone_radius"`
		IcloudLat         *float64 `json:"icloud_lat"`
		IcloudLon         *float64 `json:"icloud_lon"`
		IcloudGPSAccuracy float64  `json:"icloud_gps_accuracy"`
		IcloudLastTS      float64  `json:"icloud_last_ts"` // epoch seconds of the iCloud3 fix
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // best-effort; empty body is fine

	gateDelay, unlockDur, accGate, trigRad := s.configSnapshot()
	ev := event{
		ts: time.Now(), source: source,
		gateDelayS: gateDelay, unlockDurS: unlockDur, accGateM: accGate, trigRadM: trigRad,
	}
	if body.Lat != nil && body.Lon != nil {
		ev.lat, ev.lon = *body.Lat, *body.Lon
		ev.gpsAccuracy = body.GPSAccuracy
		ev.zoneRadiusM = body.HAZoneRadius
		ev.distanceM = haversineMeters(*body.Lat, *body.Lon, s.homeLat, s.homeLon)
		ev.hasPos = true
	}
	// iCloud3 second opinion. lat/lon default to 0 when the tracker is
	// unavailable; treat (0,0) as "no fix". Age is computed from the fix epoch.
	if body.IcloudLat != nil && body.IcloudLon != nil && *body.IcloudLat != 0 && *body.IcloudLon != 0 {
		ev.icloudLat, ev.icloudLon = *body.IcloudLat, *body.IcloudLon
		ev.icloudAccuracy = body.IcloudGPSAccuracy
		ev.icloudDistanceM = haversineMeters(*body.IcloudLat, *body.IcloudLon, s.homeLat, s.homeLon)
		if body.IcloudLastTS > 0 {
			ev.icloudAgeS = float64(time.Now().Unix()) - body.IcloudLastTS
			if ev.icloudAgeS < 0 {
				ev.icloudAgeS = 0
			}
		} else {
			ev.icloudAgeS = icloudFreshWindow.Seconds() + 1 // unknown fix time => treat as stale
		}
		ev.hasIcloud = true
		if ev.hasPos {
			ev.srcDisagreeM = haversineMeters(ev.lat, ev.lon, *body.IcloudLat, *body.IcloudLon)
		}
	}
	// Server-side iCloud3 veto: on an automated fire, if a FRESH iCloud3 fix (the
	// more reliable source) clearly disagrees and says we're outside the trigger
	// radius, the companion GPS glitched us home while still far — skip the unlock.
	// Fails open: no veto when iCloud3 is stale, missing, or roughly agrees.
	if source == "auto" && crossFresh(ev) && ev.srcDisagreeM > sourceDisagreeThresholdM && ev.icloudDistanceM > trigRad {
		ev.source = "auto_vetoed"
		s.logEvent(ev)
		slog.Info("auto unlock vetoed by iCloud3 (companion GPS glitch)",
			"companion_dist_m", math.Round(ev.distanceM),
			"icloud_dist_m", math.Round(ev.icloudDistanceM),
			"trigger_radius_m", trigRad,
			"icloud_age_s", math.Round(ev.icloudAgeS))
		writeJSON(w, http.StatusAccepted, map[string]any{
			"message":              "vetoed: iCloud3 places you outside the trigger radius",
			"source":               "auto_vetoed",
			"companion_distance_m": math.Round(ev.distanceM),
			"icloud_distance_m":    math.Round(ev.icloudDistanceM),
		})
		return
	}

	s.logEvent(ev)

	s.startSequence()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"message":             "sequence started",
		"source":              source,
		"gate_delay_sec":      gateDelay,
		"unlock_duration_sec": unlockDur,
	})
}

// handleConfig updates runtime configuration.
// POST /config  {"gate_delay_seconds": 45}
func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GateDelaySec      *float64 `json:"gate_delay_seconds"`
		UnlockDurationSec *float64 `json:"unlock_duration_seconds"`
		AccuracyGateM     *float64 `json:"gps_accuracy_max"`
		TriggerRadiusM    *float64 `json:"gate_trigger_radius"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if body.GateDelaySec != nil && *body.GateDelaySec < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "gate_delay_seconds must be >= 0"})
		return
	}
	if body.UnlockDurationSec != nil && *body.UnlockDurationSec <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unlock_duration_seconds must be > 0"})
		return
	}
	if body.AccuracyGateM != nil && *body.AccuracyGateM <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "gps_accuracy_max must be > 0"})
		return
	}
	if body.TriggerRadiusM != nil && *body.TriggerRadiusM <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "gate_trigger_radius must be > 0"})
		return
	}
	s.configMu.Lock()
	if body.GateDelaySec != nil {
		s.gateDelaySec = *body.GateDelaySec
	}
	if body.UnlockDurationSec != nil {
		s.unlockDurationSec = *body.UnlockDurationSec
	}
	if body.AccuracyGateM != nil {
		s.accuracyGateM = *body.AccuracyGateM
	}
	if body.TriggerRadiusM != nil {
		s.triggerRadiusM = *body.TriggerRadiusM
	}
	gateDelay := s.gateDelaySec
	unlockDuration := s.unlockDurationSec
	accGate := s.accuracyGateM
	trigRad := s.triggerRadiusM
	s.configMu.Unlock()
	slog.Info("config updated",
		"gate_delay_seconds", gateDelay, "unlock_duration_seconds", unlockDuration,
		"gps_accuracy_max", accGate, "gate_trigger_radius", trigRad)
	writeJSON(w, http.StatusOK, map[string]any{
		"gate_delay_seconds":      gateDelay,
		"unlock_duration_seconds": unlockDuration,
		"gps_accuracy_max":        accGate,
		"gate_trigger_radius":     trigRad,
	})
}

// handleArrivalLog records GPS position at the moment the HA unlock automation fires.
// POST /arrivals/log  {"lat":41.9,"lon":-87.6,"gps_accuracy":15,"ha_zone_radius":150}
// Appends one CSV row to arrivalLogPath; creates the file with a header if needed.
func (s *server) handleArrivalLog(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lat          float64 `json:"lat"`
		Lon          float64 `json:"lon"`
		GPSAccuracy  float64 `json:"gps_accuracy"`
		HAZoneRadius float64 `json:"ha_zone_radius"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	distM := haversineMeters(body.Lat, body.Lon, s.homeLat, s.homeLon)

	s.arrivalLogMu.Lock()
	defer s.arrivalLogMu.Unlock()

	needHeader := false
	if _, err := os.Stat(s.arrivalLogPath); os.IsNotExist(err) {
		needHeader = true
	}
	f, err := os.OpenFile(s.arrivalLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Error("arrival log: open failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not open log"})
		return
	}
	defer f.Close()

	w2 := csv.NewWriter(f)
	if needHeader {
		w2.Write([]string{"fired_at", "lat", "lon", "gps_accuracy_m", "distance_from_home_m", "ha_zone_radius_m"})
	}
	w2.Write([]string{
		time.Now().UTC().Format(time.RFC3339),
		fmt.Sprintf("%.6f", body.Lat),
		fmt.Sprintf("%.6f", body.Lon),
		fmt.Sprintf("%.1f", body.GPSAccuracy),
		fmt.Sprintf("%.1f", distM),
		fmt.Sprintf("%.0f", body.HAZoneRadius),
	})
	w2.Flush()

	slog.Info("arrival logged",
		"dist_m", math.Round(distM),
		"gps_accuracy_m", body.GPSAccuracy,
		"ha_zone_radius_m", body.HAZoneRadius,
	)
	w.WriteHeader(http.StatusNoContent)
}

// handleArrivalsGet returns the arrivals CSV as a JSON array for easy inspection.
// GET /arrivals
func (s *server) handleArrivalsGet(w http.ResponseWriter, r *http.Request) {
	s.arrivalLogMu.Lock()
	f, err := os.Open(s.arrivalLogPath)
	s.arrivalLogMu.Unlock()
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not open log"})
		return
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) < 2 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	headers := rows[0]
	out := make([]map[string]string, 0, len(rows)-1)
	for _, row := range rows[1:] {
		m := make(map[string]string, len(headers))
		for i, h := range headers {
			if i < len(row) {
				m[h] = row[i]
			}
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEventsGet returns events.csv as a JSON array for inspection.
// GET /events
func (s *server) handleEventsGet(w http.ResponseWriter, r *http.Request) {
	s.eventLogMu.Lock()
	f, err := os.Open(s.eventLogPath)
	s.eventLogMu.Unlock()
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not open log"})
		return
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) < 2 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	headers := rows[0]
	out := make([]map[string]string, 0, len(rows)-1)
	for _, row := range rows[1:] {
		m := make(map[string]string, len(headers))
		for i, h := range headers {
			if i < len(row) {
				m[h] = row[i]
			}
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleLocation receives OwnTracks HTTP-mode pings and triggers geofenced unlocks.
// POST /location  (set OwnTracks → HTTP → URL to this endpoint)
func (s *server) handleLocation(w http.ResponseWriter, r *http.Request) {
	var payload owntracksPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if payload.Type != "location" {
		// OwnTracks sends other event types (_type: "transition", etc.) — ignore them.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	dist := haversineMeters(payload.Lat, payload.Lon, s.homeLat, s.homeLon)
	inside := dist <= s.radiusM

	slog.Info("location ping",
		"lat", payload.Lat,
		"lon", payload.Lon,
		"dist_m", math.Round(dist),
		"inside", inside,
	)

	s.approach.mu.Lock()
	wasOutside := s.approach.wasOutside
	lastUnlocked := s.approach.lastUnlockedAt
	s.approach.wasOutside = !inside
	s.approach.mu.Unlock()

	switch {
	case inside && wasOutside:
		// Just entered the geofence.
		if time.Since(lastUnlocked) < s.cooldown {
			slog.Info("geofence entered but cooldown active, skipping unlock",
				"cooldown_remaining", s.cooldown-time.Since(lastUnlocked))
		} else {
			slog.Info("geofence entered — starting unlock sequence")
			gateDelay, unlockDur, accGate, trigRad := s.configSnapshot()
			s.logEvent(event{
				ts: time.Now(), source: "geofence",
				lat: payload.Lat, lon: payload.Lon, distanceM: dist, hasPos: true,
				gateDelayS: gateDelay, unlockDurS: unlockDur, accGateM: accGate, trigRadM: trigRad,
			})
			s.startSequence()
		}

	case !inside:
		// Left (or still outside) — cancel any in-flight delayed step.
		s.approach.mu.Lock()
		if s.approach.cancelSeq != nil {
			slog.Info("left geofence — cancelling pending unlock steps")
			s.approach.cancelSeq()
			s.approach.cancelSeq = nil
		}
		s.approach.mu.Unlock()
	}

	w.WriteHeader(http.StatusNoContent)
}

// startSequence fires the unlock sequence in a goroutine, cancelling any
// previously running sequence first. Each door is re-unlocked on a 3-second
// ticker for unlockDurationSec to keep it open continuously.
func (s *server) startSequence() {
	s.approach.mu.Lock()
	if s.approach.cancelSeq != nil {
		s.approach.cancelSeq()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.approach.cancelSeq = cancel
	s.approach.mu.Unlock()

	go func() {
		defer func() {
			s.approach.mu.Lock()
			s.approach.cancelSeq = nil
			s.approach.mu.Unlock()
		}()

		s.configMu.RLock()
		gateDelay := time.Duration(s.gateDelaySec * float64(time.Second))
		unlockDuration := time.Duration(s.unlockDurationSec * float64(time.Second))
		s.configMu.RUnlock()

		steps := make([]unlockStep, len(s.sequence))
		copy(steps, s.sequence)
		if len(steps) > 1 {
			steps[1].delay = gateDelay
		}

		var wg sync.WaitGroup
		for i, step := range steps {
			if step.delay > 0 {
				slog.Info("waiting before next unlock step",
					"step", i+1,
					"door", step.name,
					"delay", step.delay,
				)
				select {
				case <-time.After(step.delay):
				case <-ctx.Done():
					slog.Info("unlock sequence cancelled", "at_step", i+1, "door", step.name)
					wg.Wait()
					return
				}
			}

			slog.Info("unlocking door", "door", step.name, "id", step.doorID)
			if err := s.unlock(ctx, step.doorID, step.name); err != nil {
				slog.Error("unlock failed", "door", step.name, "err", err)
			}

			// Hold this door open by re-unlocking on a ticker for its hold duration.
			wg.Add(1)
			step := step
			doorHold := unlockDuration
			if step.holdDuration > 0 {
				doorHold = step.holdDuration
			}
			holdCtx, holdCancel := context.WithTimeout(ctx, doorHold)
			go func() {
				defer wg.Done()
				defer holdCancel()
				ticker := time.NewTicker(step.reLockInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if err := s.unlock(holdCtx, step.doorID, step.name); err != nil {
							if holdCtx.Err() != nil {
								return
							}
							slog.Error("re-unlock failed", "door", step.name, "err", err)
						}
					case <-holdCtx.Done():
						return
					}
				}
			}()
		}

		s.approach.mu.Lock()
		s.approach.lastUnlockedAt = time.Now()
		s.approach.mu.Unlock()
		slog.Info("unlock sequence complete, holding doors open", "duration", unlockDuration)

		wg.Wait()
		slog.Info("hold period complete")
	}()
}

// unlock calls the ButterflyMX API to unlock a single door.
func (s *server) unlock(ctx context.Context, doorID butterflymx.ID, name string) error {
	if name == "" {
		name = strconv.Itoa(int(doorID))
	}
	err := s.client.UnlockDoor(ctx, s.tenantID, doorID)
	if err != nil {
		return err
	}
	slog.Info("door unlocked", "door", name, "id", doorID)
	return nil
}

// auth wraps a handler with X-API-Key authentication.
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if key != s.apiKey {
			ip := r.Header.Get("CF-Connecting-IP")
			if ip == "" {
				ip = r.RemoteAddr
			}
			slog.Warn("auth failed", "ip", ip, "path", r.URL.Path)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "ip", clientIP(r))
		next(w, r)
	}
}

// resolveTenant fetches the first tenant's IDs from the API.
func resolveTenant(ctx context.Context, client *butterflymx.APIClient) (butterflymx.ID, butterflymx.TaggedID, error) {
	for tenant, err := range client.Tenants(ctx) {
		if err != nil {
			return 0, butterflymx.TaggedID{}, err
		}
		return tenant.ID.Number, tenant.ID, nil
	}
	return 0, butterflymx.TaggedID{}, nil
}

// haversineMeters returns the great-circle distance in meters between two lat/lon points.
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const earthR = 6_371_000.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return earthR * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

// defaultReLockInterval is used when a DOORS entry omits relock_s. It must be
// shorter than the door's hardware lock duration.
const defaultReLockInterval = 9 * time.Second

// parseDoors parses DOORS: a comma-separated list of doors in unlock order,
// each "id[:name[:relock_s[:hold_s]]]". relock_s is how often the door is
// re-unlocked while held (must be < its hardware lock duration); hold_s
// overrides the global unlock duration for that door. The second door, if any,
// is staggered by gate_delay.
//
//	DOORS="1001:Front Door:9:90,1002:2nd Gate:17"
func parseDoors(raw string) ([]unlockStep, error) {
	var steps []unlockStep
	for i, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		f := strings.Split(entry, ":")
		if len(f) > 4 {
			return nil, fmt.Errorf("door %d %q: want id[:name[:relock_s[:hold_s]]]", i+1, entry)
		}
		id, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("door %d %q: invalid id", i+1, entry)
		}
		step := unlockStep{doorID: butterflymx.ID(id), name: f[0], reLockInterval: defaultReLockInterval}
		if len(f) > 1 && strings.TrimSpace(f[1]) != "" {
			step.name = strings.TrimSpace(f[1])
		}
		if len(f) > 2 && strings.TrimSpace(f[2]) != "" {
			v, err := strconv.ParseFloat(strings.TrimSpace(f[2]), 64)
			if err != nil || v <= 0 {
				return nil, fmt.Errorf("door %d %q: invalid relock_s", i+1, entry)
			}
			step.reLockInterval = time.Duration(v * float64(time.Second))
		}
		if len(f) > 3 && strings.TrimSpace(f[3]) != "" {
			v, err := strconv.ParseFloat(strings.TrimSpace(f[3]), 64)
			if err != nil || v < 0 {
				return nil, fmt.Errorf("door %d %q: invalid hold_s", i+1, entry)
			}
			step.holdDuration = time.Duration(v * float64(time.Second))
		}
		steps = append(steps, step)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("no doors configured")
	}
	return steps, nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required env var not set", "key", key)
		os.Exit(1)
	}
	return v
}

func mustFloat(key string) float64 {
	s := mustEnv(key)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Error("env var is not a valid number", "key", key, "value", s)
		os.Exit(1)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// sourceOr returns the ?source= query param, or def if absent.
func sourceOr(r *http.Request, def string) string {
	if v := r.URL.Query().Get("source"); v != "" {
		return v
	}
	return def
}

func floatEnvOr(key string, def float64) float64 {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}
