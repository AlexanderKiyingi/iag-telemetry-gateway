-- Keep in sync with fleet db/migrations/0015_telemetry_trips_geofence.sql

DELETE FROM telemetry_timeseries a
    USING telemetry_timeseries b
WHERE a.vehicle_id = b.vehicle_id
  AND a.ts = b.ts
  AND a.ctid < b.ctid;

CREATE UNIQUE INDEX IF NOT EXISTS telemetry_timeseries_vehicle_ts_uidx
    ON telemetry_timeseries (vehicle_id, ts);

CREATE TABLE IF NOT EXISTS vehicle_geofence_state (
    vehicle_id  TEXT NOT NULL,
    poi_name    TEXT NOT NULL,
    inside      BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (vehicle_id, poi_name)
);

ALTER TABLE trips
    ADD COLUMN IF NOT EXISTS started_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ended_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS auto_generated BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS trips_started_at_idx ON trips (vehicle_id, started_at DESC);
