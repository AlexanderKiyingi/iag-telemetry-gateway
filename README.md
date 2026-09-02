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

## Fuel sensors

Which IO element carries fuel, and how its raw units become a percentage, is
configured **per device** (`iot_devices.fuel_io_id` / `fuel_scale` /
`fuel_offset`, fleet migration 0047). It used to be one hardcoded constant —
Teltonika IO 89, CAN fuel level in tenths of a percent — which exists only on a
unit wired to a CAN adapter, so the two sensors most often fitted to a truck
were unreadable.

    fuel_percent = raw_value * fuel_scale + fuel_offset   (clamped 0-100)

| Sensor | `fuel_io_id` | `fuel_scale` | `fuel_offset` |
|--------|-------------|--------------|---------------|
| CAN fuel level, percent (default) | `89` | `0.1` | `0` |
| LLS capacitive probe, 0-4095 | `201` | `0.024420` | `0` |
| Analog sender on AIN1, 0.5-4.5 V (mV) | `9` | `0.025` | `-12.5` |
| No fuel sensor | `0` | — | — |

Confirm the id against the unit's own IO list before relying on the table:
numbering varies by model, firmware and CAN adapter. Set it at registration or
later — calibration is normally measured against a known tank level once the
sensor is in the vehicle:

```sh
PATCH /api/iot/devices/:id  { "fuelIoId": 201, "fuelScale": 0.024420 }
```

The defaults reproduce the previous hardcoded behaviour, so an existing fleet
reads exactly as it did until a device is deliberately reconfigured.

Every fixed-width IO element the device sends is stored in
`telemetry_timeseries.raw` regardless of configuration, and since Codec 8E
variable-length elements are kept too, a mapping corrected later can be
backfilled from history rather than needing the fleet re-driven.

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
