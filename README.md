# Fleet_IoT

Fleet telemetry ingest service for the IAG platform. **iag-fleet** owns business APIs, device registry UI, aggregation jobs, and schema migrations; **Fleet_IoT** owns high-throughput ingest into the **`telemetry_timeseries`** TimescaleDB hypertable.

## Architecture

```text
Devices / relays
    ├─ TCP Teltonika (:5027)  → cmd/gateway
    ├─ TCP SinoTrack/HQ (:5013) → cmd/sinotrack   (ST-901/906/915 + HQ clones)
    └─ HTTP JSON (:4080)      → cmd/ingest
              │
              ▼
    telemetry_timeseries  (TimescaleDB hypertable on ts)
              │
              ▼
    Redis pub/sub (optional) → fleet SSE live map
```

| Component | Repo | Responsibility |
|-----------|------|----------------|
| **Fleet_IoT** | this repo (`edge/Fleet_IoT`) | Ingest only |
| **iag-fleet** | `services/operations/fleet` | Reads, device admin, jobs, migrations |

Go module path: `github.com/iag/fleet-iot` (import path). GitHub / folder name: **Fleet_IoT**.

## Timescale table

Raw pings are stored in **`telemetry_timeseries`**, partitioned by `ts` via TimescaleDB (`create_hypertable`). Legacy **`telemetry_pings`** is migrated and dropped by fleet migration `0010_telemetry_timeseries.sql`.

Requires Postgres with the Timescale extension (see `deploy/postgres/init/00-timescale.sql` and `timescale/timescaledb` image in Compose).

## Binaries

| Binary | Default | Description |
|--------|---------|-------------|
| `/app/ingest` | `:4080` | `POST /v1/pings`, `POST /api/iot/pings` |
| `/app/gateway` | `:5027` | Teltonika Codec 8/8E TCP |
| `/app/sinotrack` | `:5013` | SinoTrack / HQ-protocol TCP (ST-901/906/915 + GT06-era clones) |

## Environment

| Variable | Required | Notes |
|----------|----------|-------|
| `DATABASE_URL` | yes | Telemetry Timescale (`telemetry_timeseries`, aggregates) |
| `REGISTRY_DATABASE_URL` | split-DB only | Operational fleet DB — `iot_devices`, `SyncVehicleFromPing` on `vehicles` (same DSN as fleet `DATABASE_URL`) |
| `EVENT_BUS_ENABLED` | no | When `true`, ingest enqueues `fleet.vehicle.status_changed` to `fleet_event_outbox` on the operational DB |
| `REDIS_URL` | no | Live SSE fan-out for fleet API replicas |
| `ADDR` / `PORT` | no | HTTP ingest (default `:4080`) |
| `IOT_ADDR` | no | Teltonika TCP gateway (default `:5027`) |
| `SINOTRACK_ADDR` | no | SinoTrack/HQ TCP gateway (default `:5013`) |

## Smoke-testing SinoTrack without hardware

`cmd/hqreplay` is a dev-only TCP client that feeds HQ frames to a running
`cmd/sinotrack` gateway. Register a device whose `serial` matches `-id`, then:

```sh
# synthetic stream that drifts NE around Kampala
go run ./cmd/hqreplay -addr localhost:5013 -id 9170503816 -count 10

# replay captured frames verbatim (one per line; blank/'#'-prefixed lines skipped)
go run ./cmd/hqreplay -addr localhost:5013 -file captured-frames.txt
```

It is **not** part of the Docker build — local tooling only.

## Monorepo wiring

```go
// services/operations/fleet/go.mod
require github.com/iag/fleet-iot v0.0.0
replace github.com/iag/fleet-iot => ../../../edge/Fleet_IoT
```

## Standalone remote

`https://github.com/AlexanderKiyingi/Fleet_IoT.git` — publish this directory and depend on a tagged `github.com/iag/fleet-iot` release from fleet.
