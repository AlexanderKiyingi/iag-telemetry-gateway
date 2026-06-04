package iot

import "fmt"

// SQL fragments for telemetry_timeseries (Timescale hypertable). Use PingsTable
// so renames stay in one place.
var (
	sqlFromPings = fmt.Sprintf("FROM %s", PingsTable)

	sqlSelectPingCols = "vehicle_id, device_id, ts, lat, lng, altitude, heading, speed_kmh, satellites, odo, fuel_level, ignition, event_id, raw"

	sqlLatestPing = fmt.Sprintf(`
        SELECT %s
        %s WHERE vehicle_id = $1
        ORDER BY ts DESC LIMIT 1`, sqlSelectPingCols, sqlFromPings)

	sqlTrackPings = fmt.Sprintf(`
        SELECT %s
        %s
        WHERE vehicle_id = $1 AND ts BETWEEN $2 AND $3
        ORDER BY ts ASC LIMIT $4`, sqlSelectPingCols, sqlFromPings)

	sqlTrackPingsAfter = fmt.Sprintf(`
        SELECT %s
        %s
        WHERE vehicle_id = $1 AND ts > $2 AND ts BETWEEN $3 AND $4
        ORDER BY ts ASC LIMIT $5`, sqlSelectPingCols, sqlFromPings)

	sqlPingsForDay = fmt.Sprintf(`
        SELECT ts, lat, lng, speed_kmh, fuel_level, ignition
        %s
        WHERE vehicle_id = $1 AND ts >= $2 AND ts < $3
        ORDER BY ts`, sqlFromPings)

	sqlDistinctVehicleDays = fmt.Sprintf(`
        SELECT DISTINCT vehicle_id, (ts AT TIME ZONE 'UTC')::date AS day
        %s
        WHERE ts >= $1 AND ts < $2
        ORDER BY vehicle_id, day`, sqlFromPings)

	sqlFuelHistory = fmt.Sprintf(`
        SELECT ts, lat, lng, speed_kmh, fuel_level, ignition, odo
        %s
        WHERE vehicle_id = $1 AND ts BETWEEN $2 AND $3 AND fuel_level IS NOT NULL
        ORDER BY ts ASC LIMIT $4`, sqlFromPings)

	sqlPurgeBefore = fmt.Sprintf("DELETE FROM %s WHERE ts < $1", PingsTable)

	sqlListDaily = `
        SELECT vehicle_id, day, ping_count, distance_km, max_speed_kmh, avg_speed_kmh,
               fuel_used_litres, moving_minutes, idle_minutes, first_ping, last_ping
        FROM telemetry_daily
        WHERE vehicle_id = $1 AND day >= $2::date AND day <= $3::date
        ORDER BY day ASC`

	sqlInsertPing = fmt.Sprintf(`
        INSERT INTO %s (
            vehicle_id, device_id, ts, lat, lng, altitude, heading, speed_kmh,
            satellites, odo, fuel_level, ignition, event_id, raw
        ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
        ON CONFLICT (vehicle_id, ts) DO NOTHING`, PingsTable)
)
