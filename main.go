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
	wasOutside     bool       // true when last known position was outside geofence
	cancelSeq      func()     // cancels in-flight delayed unlock sequence, nil if none
}

type server struct {
	client             *butterflymx.APIClient
	tenantID           butterflymx.ID
	tenantTagID        butterflymx.TaggedID
	apiKey             string
	homeLat            float64
	homeLon            float64
	radiusM            float64
	cooldown           time.Duration
	sequence           []unlockStep
	approach           approachState
	configMu           sync.RWMutex
	gateDelaySec       float64
	unlockDurationSec  float64
	arrivalLogMu       sync.Mutex
	arrivalLogPath     string
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

	client := butterflymx.NewAPIClient(butterflymx.APIStaticToken(apiToken), nil)

	ctx := context.Background()
	tenantID, tenantTagID, err := resolveTenant(ctx, client)
	if err != nil {
		slog.Error("failed to resolve tenant", "err", err)
		os.Exit(1)
	}
	slog.Info("tenant resolved", "id", tenantID)

	arrivalLogPath := "arrivals.csv"
	if p := os.Getenv("ARRIVAL_LOG_PATH"); p != "" {
		arrivalLogPath = p
	}

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
		arrivalLogPath:    arrivalLogPath,
		sequence: []unlockStep{
			{doorID: 13723, name: "Front Door", delay: 0, reLockInterval: 9 * time.Second, holdDuration: 30 * time.Second}, // hardware lock: 10s; user clears front gate well within 30s
			{doorID: 15238, name: "2nd Gate", delay: 0, reLockInterval: 17 * time.Second},                                 // hardware lock: 20s; holds for full unlockDuration
		},
		approach: approachState{wasOutside: true},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /doors", s.auth(s.handleDoors))
	mux.HandleFunc("GET /status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /arrivals", s.auth(s.handleArrivalsGet))
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
		"cooldown_min", cooldownMin,
	)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// handleDoors returns the static door list.
func (s *server) handleDoors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"doors": []map[string]any{
			{"id": "13723", "name": "Front Door", "online": true},
			{"id": "15238", "name": "2nd Gate", "online": true},
		},
	})
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
// POST /unlock/13723
// POST /unlock/13723,15238
func (s *server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("doors")
	parts := strings.Split(raw, ",")

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

// handleUnlockSequence triggers the configured approach sequence manually.
// POST /unlock/sequence
func (s *server) handleUnlockSequence(w http.ResponseWriter, r *http.Request) {
	s.startSequence()
	s.configMu.RLock()
	delay := s.gateDelaySec
	duration := s.unlockDurationSec
	s.configMu.RUnlock()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"message":             "sequence started",
		"gate_delay_sec":      delay,
		"unlock_duration_sec": duration,
	})
}

// handleConfig updates runtime configuration.
// POST /config  {"gate_delay_seconds": 45}
func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GateDelaySec      *float64 `json:"gate_delay_seconds"`
		UnlockDurationSec *float64 `json:"unlock_duration_seconds"`
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
	s.configMu.Lock()
	if body.GateDelaySec != nil {
		s.gateDelaySec = *body.GateDelaySec
	}
	if body.UnlockDurationSec != nil {
		s.unlockDurationSec = *body.UnlockDurationSec
	}
	gateDelay := s.gateDelaySec
	unlockDuration := s.unlockDurationSec
	s.configMu.Unlock()
	slog.Info("config updated", "gate_delay_seconds", gateDelay, "unlock_duration_seconds", unlockDuration)
	writeJSON(w, http.StatusOK, map[string]any{
		"gate_delay_seconds":      gateDelay,
		"unlock_duration_seconds": unlockDuration,
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
