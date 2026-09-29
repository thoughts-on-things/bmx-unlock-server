# bmx-unlock-server

Small Go server that unlocks ButterflyMX doors when I arrive home. Home Assistant
(or OwnTracks) posts location updates; when I cross into the geofence the server
runs an unlock sequence and holds each door open by re-unlocking it before the
hardware relocks. An auto-tuner reviews logged arrivals and adjusts the hold and
gate-delay settings over time.

## Build

Needs Go 1.25+ with `GOEXPERIMENT=jsonv2`:

```sh
make build    # linux/arm64 binary for the Home Assistant host
go test ./...
```

`go.mod` has a `replace` pointing `libdb.so/go-butterflymx` at a local checkout
(`../go-butterflymx`). Clone that library next to this repo, or remove the
`replace` line to use the published module.

## Configuration

Set these as environment variables or in a `.env` file (`cp .env.example .env`). Don't commit `.env`.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `BUTTERFLYMX_API_TOKEN` | yes | | ButterflyMX API token |
| `API_KEY` | yes | | Shared key clients send as `X-API-Key` (or `?key=`) |
| `HOME_LAT`, `HOME_LON` | yes | | Geofence center |
| `DOORS` | yes | | Doors to unlock, in order (see below) |
| `GEOFENCE_RADIUS_M` | | 200 | Geofence radius |
| `COOLDOWN_MINUTES` | | 10 | Minimum time between automatic unlocks |
| `GATE_DELAY_SECONDS` | | 30 | Delay before the second gate unlocks |
| `UNLOCK_DURATION_SECONDS` | | 30 | Default hold duration |
| `ACCURACY_GATE_M` | | 100 | GPS accuracy gate (enforced in HA; logged for tuning) |
| `TRIGGER_RADIUS_M` | | 100 | Arrival trigger radius (enforced in HA; logged for tuning) |
| `HA_URL` | | `http://localhost:8123` | Home Assistant URL for the tuner |
| `HA_TOKEN` | | | HA long-lived token. Without it the tuner only recommends changes and doesn't apply them |
| `TUNE_INTERVAL_HOURS` | | 6 | How often the tuner runs |
| `TUNE_WINDOW_DAYS` | | 14 | How much history the tuner looks at |
| `ARRIVAL_LOG_PATH`, `EVENT_LOG_PATH`, `TUNER_STATE_PATH`, `TUNING_LOG_PATH` | | `arrivals.csv`, `events.csv`, `tuner_state.json`, `tuning.log` | Where runtime data is written |

### Doors

`DOORS` is a comma-separated list of one or more doors, unlocked in the order
listed. Each entry is `id[:name[:relock_s[:hold_s]]]`:

- `id`: ButterflyMX door ID. `GET /status` lists your building's doors.
- `name`: label for logs and `/doors`. Defaults to the ID.
- `relock_s`: how often the door is re-unlocked while being held open. Must be
  shorter than the door's hardware relock time. Defaults to 9.
- `hold_s`: how long to hold this door open. Defaults to `UNLOCK_DURATION_SECONDS`.

The second door, if there is one, waits `GATE_DELAY_SECONDS` before it unlocks.
Any doors after that unlock right after the second one.

```sh
DOORS="1001:Front Door:9:90,1002:2nd Gate:17"
```

## API

Every endpoint requires the API key.

| Method | Path | |
|---|---|---|
| `POST` | `/location` | Location update. Triggers the sequence on arrival |
| `POST` | `/unlock/sequence` | Run the full unlock sequence |
| `POST` | `/unlock/{doors}` | Unlock specific doors |
| `POST` | `/config` | Update tunable settings |
| `POST` | `/tune/run` | Run the tuner now |
| `POST` | `/arrivals/log` | Record an arrival |
| `GET` | `/doors`, `/status`, `/arrivals`, `/events`, `/tuning` | Inspect state |

## Deploy

`make deploy` copies the binary to the Home Assistant host and restarts the
server through a webhook. `make logs` follows the server log.
