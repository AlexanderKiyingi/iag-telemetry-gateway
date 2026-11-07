# Onboarding SeeWorld GPS trackers

How to bring **any SeeWorld GPS model** online against the IAG fleet platform.

SeeWorld units speak one of two wire protocols:

| Protocol | Listener | Default port | Typical models |
|----------|----------|--------------|----------------|
| **GT06** (binary `7878`/`7979`) | `/app/gt06` | `:5023` | S06A, S5L, S208(L), R11/R12L/R16/R31(L), R56(L)/R58L, S11/S21L, W15/W18L/W21L, D/G series, P2, … |
| **JT808** (`7E` framing) | `/app/jt808` | `:5015` | P5, W20L |

Dash cams (V6 / V7 family) are **not** part of this GPS TCP path.

```text
SeeWorld GT06 ──TCP──▶  fleet-iot-gt06   (:5023) ──▶ Pipeline.Ingest
SeeWorld JT808 ─TCP──▶  fleet-iot-jt808  (:5015) ──▶ Pipeline.Ingest
```

> These speak raw TCP, **not** HTTP. They cannot go through the api-gateway.
> Do **not** point them at `/app/sinotrack` (:5013) — that listener only accepts
> HQ ASCII `*HQ,…#` frames.

---

## 1. Deploy the listener

Same image as HTTP ingest (`edge/Fleet_IoT`). Override the start command:

| Compose service | Command | Port |
|-----------------|---------|------|
| `fleet-iot-gt06` | `/app/gt06` | 5023 |
| `fleet-iot-jt808` | `/app/jt808` | 5015 |

On Railway: separate TCP Proxy services with
`startCommand = "/app/gt06"` or `"/app/jt808"`. Prefer a **literal public IP** —
many firmwares reject hostnames in the SERVER SMS.

Set on **iag-fleet** so the ingestion guide can show operators the SMS:

```
SEEWORLD_GT06_PUBLIC_ADDR=<ip>:5023
SEEWORLD_JT808_PUBLIC_ADDR=<ip>:5015
```

---

## 2. Register the device

`POST /api/iot/devices` with catalogue hardware (brand `SeeWorld`, model from
`GET /api/iot/catalog`):

```json
{
  "serial": "<IMEI or JT808 terminal phone digits>",
  "label": "Truck 12 tracker",
  "vehicleId": "<uuid>",
  "deviceType": "gps_tracker",
  "brand": "SeeWorld",
  "model": "S5L",
  "issueKey": false
}
```

- **GT06**: `serial` = IMEI (usually 15 digits). The gateway also tries common
  zero-pad variants when matching `iot_devices.serial`.
- **JT808**: `serial` = terminal phone / IMEI digits in the JT808 header
  (leading zeros usually trimmed; pad variants are tried on connect).

---

## 3. Program the unit (SMS)

Typical SeeWorld / Concox SERVER command (also returned by
`GET /api/iot/ingestion` → `tcp.gt06.sms` / `tcp.jt808.sms`):

```text
SERVER,0,<ip>,<port>,0#
```

Hostname form (if the firmware accepts it):

```text
SERVER,1,<hostname>,<port>,0#
```

Set the carrier APN first (exact APN command varies by firmware — check the
unit manual). Then `RESET#` if the device does not reconnect on its own.

---

## 4. Verify

1. Gateway logs: `gt06 device connected` / `jt808 device connected`.
2. `iot_devices.protocol` becomes `gt06` or `jt808`.
3. `GET /api/vehicles/:id/track/latest` returns a fix; live map moves.

---

## Notes

- Immobilise / fuel-cut downlink is **not** enabled until a per-model encoder is
  verified on hardware (`FLEET_DEVICE_COMMANDS_ENABLED` alone is not enough).
- Fuel IO defaults to disabled (`fuel_io_id=0`) for SeeWorld creates — these
  trackers are not Teltonika CAN hosts.
- If a future SKU speaks GT06 or JT808, add a catalogue row only; no new parser.
