# Fleet_IoT

Fleet telemetry ingest service for the IAG platform. **iag-fleet** owns business APIs, device registry UI, aggregation jobs, and schema migrations; **Fleet_IoT** owns high-throughput ingest into the **`telemetry_timeseries`** TimescaleDB hypertable.

## Architecture

```text
Devices / relays
    ├─ TCP Teltonika (:5027)  → cmd/gateway
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

## Environment

| Variable | Required | Notes |
|----------|----------|-------|
| `DATABASE_URL` | yes | Telemetry Timescale (`telemetry_timeseries`, aggregates) |
| `REGISTRY_DATABASE_URL` | split-DB only | Operational fleet DB — `iot_devices`, `SyncVehicleFromPing` on `vehicles` (same DSN as fleet `DATABASE_URL`) |
| `EVENT_BUS_ENABLED` | no | When `true`, ingest enqueues `fleet.vehicle.status_changed` to `fleet_event_outbox` on the operational DB |
| `REDIS_URL` | no | Live SSE fan-out for fleet API replicas |
| `ADDR` / `PORT` | no | HTTP ingest (default `:4080`) |
| `IOT_ADDR` | no | TCP gateway (default `:5027`) |

## Monorepo wiring

```go
// services/operations/fleet/go.mod
require github.com/iag/fleet-iot v0.0.0
replace github.com/iag/fleet-iot => ../../../edge/Fleet_IoT
```

## Standalone remote

`https://github.com/AlexanderKiyingi/Fleet_IoT.git` — publish this directory and depend on a tagged `github.com/iag/fleet-iot` release from fleet.
