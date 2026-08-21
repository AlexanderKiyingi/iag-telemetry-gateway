-- Telemetry storage policies, and the duplicate index that was costing every insert.
--
-- telemetry_timeseries was created as a hypertable in 0010 and then used none
-- of what a hypertable is for: no compression, no retention, no rollup. At
-- roughly 1.7M rows a day for a 200-vehicle fleet reporting every ten seconds —
-- each row carrying a `raw` JSONB payload — it grows uncompressed and unbounded
-- on the same Postgres every business service shares.
--
-- Keep in sync with iag-fleet db/migrations (the two repos share this database).

-- ── 1. Drop the redundant index ────────────────────────────────────────────
--
-- 0010 created (vehicle_id, ts DESC) and 0012 added a UNIQUE (vehicle_id, ts).
-- Both were maintained on every insert. Postgres reads a B-tree in either
-- direction, so the unique index answers every query the other one did —
-- latest-ping (ORDER BY ts DESC LIMIT 1), track windows, the daily aggregator —
-- and it cannot be dropped because ON CONFLICT (vehicle_id, ts) needs it.
-- So the non-unique one is the one that goes.
DROP INDEX IF EXISTS telemetry_timeseries_vehicle_ts_idx;

DO $fleet_iot_policies$
DECLARE
    compress_after  INTERVAL := INTERVAL '7 days';
    retain_after    INTERVAL := INTERVAL '180 days';
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        RAISE NOTICE 'timescaledb not installed — skipping compression/retention policies';
        RETURN;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM timescaledb_information.hypertables
         WHERE hypertable_name = 'telemetry_timeseries'
    ) THEN
        RAISE NOTICE 'telemetry_timeseries is not a hypertable — skipping policies';
        RETURN;
    END IF;

    -- ── 2. Compression ─────────────────────────────────────────────────────
    --
    -- Segment by vehicle_id: every read path filters on it, so segmenting
    -- there lets a compressed chunk be scanned for one vehicle without
    -- decompressing its neighbours. Order by ts DESC so the newest rows in a
    -- chunk decompress first, which is the direction latest-ping reads.
    --
    -- Seven days is comfortably past the window the live map, the track view
    -- and the daily aggregator work in, so nothing hot is ever compressed.
    --
    -- One caveat to know before backfilling: InsertPings uses ON CONFLICT
    -- (vehicle_id, ts) DO NOTHING, and Timescale restricts ON CONFLICT against
    -- a COMPRESSED chunk. Live ingest is unaffected — it always writes to the
    -- current chunk, which is days away from being compressed. But replaying a
    -- capture with timestamps older than compress_after (cmd/hqreplay -file
    -- with a historical file; its default synthetic stream uses current UTC and
    -- is fine) will error rather than silently drop rows. Decompress the target
    -- chunks first if you need to do that.
    BEGIN
        ALTER TABLE telemetry_timeseries SET (
            timescaledb.compress,
            timescaledb.compress_segmentby = 'vehicle_id',
            timescaledb.compress_orderby   = 'ts DESC'
        );
    EXCEPTION WHEN OTHERS THEN
        -- Already configured, or a Timescale build without compression
        -- (Apache-licensed). Neither is a reason to fail the migration.
        RAISE NOTICE 'compression not configured on telemetry_timeseries: %', SQLERRM;
    END;

    BEGIN
        PERFORM add_compression_policy('telemetry_timeseries', compress_after,
                                       if_not_exists => TRUE);
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'add_compression_policy skipped: %', SQLERRM;
    END;

    -- ── 3. Retention ───────────────────────────────────────────────────────
    --
    -- 180 days of raw pings. The daily rollup in telemetry_daily is NOT
    -- covered by this and is not dropped — history older than the retention
    -- window stays available at day resolution, which is what the fuel and
    -- distance reports read anyway.
    --
    -- Deliberately NOT unconditional: raise TELEMETRY_RETAIN_DAYS by editing
    -- this policy, not by removing it. Set it against a legal retention
    -- obligation before this reaches a jurisdiction that has one.
    BEGIN
        PERFORM add_retention_policy('telemetry_timeseries', retain_after,
                                     if_not_exists => TRUE);
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'add_retention_policy skipped: %', SQLERRM;
    END;
END
$fleet_iot_policies$;
