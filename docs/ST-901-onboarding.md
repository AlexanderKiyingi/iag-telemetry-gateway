# Onboarding SinoTrack ST-901 (and HQ-protocol clones)

How to bring a SinoTrack **ST-901 / ST-906 / ST-915** (or any GT06-era HQ-protocol
clone) online against the IAG fleet platform.

The decoder and gateway already exist (`iot/hq.go`, `cmd/sinotrack`). Onboarding is
**operational, not code** — there is no new protocol work. Three things have to be true:

1. The `sinotrack` gateway is **running** and its TCP port is **publicly reachable**.
2. The device is **registered** in `iot_devices` (serial = the id the tracker sends).
3. The device is **programmed** over SMS to dial that public host:port and upload over GPRS.

```text
ST-901 ──2G/GPRS, raw TCP──▶  fleet-iot-sinotrack  ──▶ telemetry_timeseries
  (HQ *HQ,…# frames)           (:5013, cmd/sinotrack)     hot-state · geofence · live SSE
```

> The ST-901 speaks raw TCP, **not** HTTP — it cannot go through the api-gateway or the
> Next BFF. It needs a direct, public TCP endpoint.

---

## 1. Deploy the gateway

The binary ships in the standard `edge/Fleet_IoT` image (build loop includes
`./cmd/sinotrack`; `EXPOSE … 5013`). Run it as its own process with the entrypoint
overridden — same env as the Teltonika gateway.

**Local / Compose** — already wired as `fleet-iot-sinotrack` in
[`deploy/docker-compose.yml`](../../../deploy/docker-compose.yml):

```sh
docker compose up -d fleet-iot-sinotrack      # listens on :5013
```

**Production (Railway or similar):** deploy the image with
```
command: ["/app/sinotrack"]
SINOTRACK_ADDR=":5013"
DATABASE_URL=…telemetry…   REGISTRY_DATABASE_URL=…operational…
REDIS_URL=…   EVENT_BUS_ENABLED=true
```
then expose `:5013` over **raw TCP** (Railway: add a **TCP Proxy** to the service — HTTP
routing will not work).

> ⚠️ **Static IP caveat.** The ST-901 `804` (set-server) command expects an **IP address**
> on most firmwares. Railway's TCP proxy gives a *domain*:port — fine only if your unit's
> firmware accepts a hostname. If it requires a literal IP, terminate on something with a
> stable public IP (a small VM running the gateway, or a static-IP TCP load balancer in
> front of it). Confirm with one test unit before buying in bulk.

---

## 2. Register the device

HQ devices authenticate **by serial only** — there is no API key (unlike HTTP relays).
Create the `iot_devices` row with `manage_iot_device` permission, via the Fleet IoT
Devices settings panel or the API:

```sh
curl -X POST "$FLEET/api/iot/devices" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"serial":"9170503816","label":"Truck UAX-123 ST-901","vehicleId":"VEH-001","issueKey":false}'
```

- **`serial`** must equal the **id the tracker transmits** in field `[1]` of its HQ frame
  (often the IMEI or the number printed on the label — see below). Not the SIM number.
- **`issueKey:false`** — HQ devices don't use API keys.
- Binding `vehicleId` now means hot-state, geofencing, and the live map light up on the
  first valid fix.

### Finding the transmitted id
Easiest: let the device connect once *before* registering. The gateway logs the id it
sees and closes the socket:
```
INFO unknown or inactive sinotrack device, closing  deviceId=9170503816
```
Register that exact `deviceId` as the serial, and the next connection authenticates.
(Alternatively read it from the device label / the SinoTrack app.)

---

## 3. Program the device over SMS

Insert a data-enabled SIM, power the unit, and text it these commands. **Default
password `0000`.** Replace the IP/port/APN with yours. Reply to each is `SET OK`.

| Step | Command (literal) | Example |
|------|-------------------|---------|
| Set APN | `803<pwd> <apn>` | `8030000 internet` |
| APN w/ user+pass | `803<pwd> <apn> <user> <pass>` | `8030000 internet web web` |
| Set server IP + port | `804<pwd> <ip> <port>` | `8040000 45.112.204.245 5013` |
| Moving (ACC-on) interval, sec | `805<pwd> <T>` | `8050000 15` |
| Parked (ACC-off) interval, sec | `809<pwd> <T>` | `8090000 180` |

Notes:
- Type the command with **no `+` and one real space** where the table shows a space.
- Interval `T` is seconds (`0`–`18000`); `T=0` disables GPRS for that mode.
- Use the **carrier's APN** for the SIM (e.g. Uganda: MTN `internet`, Airtel `internet`).
- After `804` the unit starts dialing the gateway; watch the logs (step 4).

> SMS syntax varies slightly across firmware revisions — if `SET OK` doesn't come back,
> check the exact command list in your unit's manual (linked at the bottom).

---

## 4. Verify

1. **Gateway log** shows the handshake and inserts:
   ```
   INFO sinotrack device connected   deviceId=9170503816 vehicleId=VEH-001
   INFO ping persisted               lat=0.3476 lng=32.5825
   ```
2. **Latest fix** via the API:
   ```sh
   curl -H "Authorization: Bearer $TOKEN" "$FLEET/api/vehicles/VEH-001/track/latest"
   ```
3. **Live map** — the vehicle moves on the fleet map; `track`/`vehicles/live` streams emit.
4. **History** accrues in `telemetry_timeseries`; trips auto-detect on the nightly job.

A `V` (no-fix) frame is logged and skipped (no 0,0 pollution); the unit needs clear sky
for its first lock.

---

## Smoke-test without hardware

`cmd/hqreplay` feeds synthetic HQ frames to a running gateway (register a device whose
serial matches `-id` first):

```sh
go run ./cmd/hqreplay -addr localhost:5013 -id 9170503816 -count 10
go run ./cmd/hqreplay -addr localhost:5013 -file captured-frames.txt
```

---

## What works vs. what doesn't (ST-901 specifics)

| Capability | Status |
|---|---|
| Live GPS — position, speed, heading | ✅ decoded → telemetry, live SSE, history |
| Trip history / replay | ✅ server-side trip detection |
| Moving/idle status | ✅ derived from speed |
| Geofencing | ✅ server-side (`ProcessGeofences` per fix) |
| ACC / ignition on-off | ⚠️ in the HQ status word; **stored raw (`hqStatus`), not decoded** into `Ping.Ignition` |
| Device alarms (vibration, power-cut, overspeed, tamper) | ⚠️ ride in V4/status frames; preserved raw, **not** turned into `safety_events` |
| Remote engine cut-off (immobilize) | ❌ needs a server→device downlink; gateway is ingest-only |
| Fuel level / theft | ❌ N/A — the ST-901 has **no fuel sensor** |
| Odometer | ❌ not in protocol; distance derived from GPS |

Decoding the HQ status word (ACC + alarms) is a small, model-specific follow-up — capture
a few real frames from a live unit first, since the bit layout varies across clones.

---

## Troubleshooting

- **`unknown or inactive sinotrack device, closing`** — serial mismatch or device
  inactive. Register the exact logged `deviceId`; ensure `is_active=true`.
- **Connects then drops** — idle read deadline is 5 min; if the unit's parked interval
  (`809`) is longer, it'll reconnect each report (normal). Heartbeat/non-position frames
  refresh last-seen and keep the link warm.
- **No connection at all** — APN wrong, port blocked, or firmware needs an IP not a
  hostname (see the static-IP caveat in step 1). Confirm the SIM has data and 2G coverage.
- **2G sunset** — the base ST-901 is 2G GSM/GPRS. Where 2G is retired, use the 4G
  variants (**ST-901L / ST-901M**); the decoder handles them identically.

---

## References
- [SinoTrack ST-901 user manual (ManualsLib)](https://www.manualslib.com/manual/1381517/Sinotrack-St-901.html)
- [ST-901 SMS commands & troubleshooting (manuals.plus)](https://manuals.plus/sinotrack/st-901-gps-tracker-manual)
- [SinoTrack HQ protocol reference (flespi)](https://flespi.com/protocols/sinotrack)
