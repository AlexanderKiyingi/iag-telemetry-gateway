-- Timescale hypertable for raw telemetry (shared schema with iag-fleet migration 0010).
-- Run against the same Postgres database fleet uses.

CREATE TABLE IF NOT EXISTS telemetry_timeseries (
    vehicle_id  TEXT NOT NULL,
    device_id   BIGINT REFERENCES iot_devices(id) ON DELETE SET NULL,
    ts          TIMESTAMPTZ NOT NULL,
    lat         DOUBLE PRECISION NOT NULL,
    lng         DOUBLE PRECISION NOT NULL,
    altitude    DOUBLE PRECISION,
    heading     DOUBLE PRECISION,
    speed_kmh   DOUBLE PRECISION,
    satellites  SMALLINT,
    odo         DOUBLE PRECISION,
    fuel_level  DOUBLE PRECISION,
    ignition    BOOLEAN,
    event_id    INTEGER,
    raw         JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS telemetry_timeseries_vehicle_ts_idx
    ON telemetry_timeseries (vehicle_id, ts DESC);

CREATE INDEX IF NOT EXISTS telemetry_timeseries_ts_brin_idx
    ON telemetry_timeseries USING BRIN (ts);

DO $fleet_iot$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM create_hypertable(
            'telemetry_timeseries',
            'ts',
            if_not_exists => TRUE,
            migrate_data => FALSE
        );
    ELSE
        RAISE NOTICE 'timescaledb extension not installed — telemetry_timeseries remains a regular table';
    END IF;
END
$fleet_iot$;
