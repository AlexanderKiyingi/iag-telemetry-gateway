package iot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrDeviceNotFound = errors.New("device not found")
	ErrInvalidAPIKey  = errors.New("invalid device api key")
	ErrInactiveDevice = errors.New("device inactive")
)

// Store holds Postgres pools for fleet telemetry and operational registry data.
// When telemetry is nil, all queries use operational (single-DB dev mode).
type Store struct {
	operational *pgxpool.Pool
	telemetry   *pgxpool.Pool
}

// NewStore wires one pool for both operational and telemetry tables (local dev).
func NewStore(pool *pgxpool.Pool) *Store {
	return NewSplitStore(pool, nil)
}

// NewSplitStore separates registry/hot-state (operational) from time-series (telemetry).
func NewSplitStore(operational, telemetry *pgxpool.Pool) *Store {
	if operational == nil {
		operational = telemetry
	}
	return &Store{operational: operational, telemetry: telemetry}
}

func (s *Store) op() *pgxpool.Pool { return s.operational }

func (s *Store) tel() *pgxpool.Pool {
	if s.telemetry != nil {
		return s.telemetry
	}
	return s.operational
}

// ─────────────────────────────── Devices ────────────────────────────────

type CreateDeviceInput struct {
	Serial    string
	Label     string
	VehicleID string
	IssueKey  bool // when true, a fresh API key is generated; the plaintext is returned once
}

type CreatedDevice struct {
	Device
	APIKeyPlaintext string `json:"apiKey,omitempty"`
}

func (s *Store) CreateDevice(ctx context.Context, in CreateDeviceInput) (*CreatedDevice, error) {
	var keyHash, plaintext string
	if in.IssueKey {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		plaintext = base64.RawURLEncoding.EncodeToString(buf)
		keyHash = hashAPIKey(plaintext)
	}
	const q = `
        INSERT INTO iot_devices (serial, label, vehicle_id, api_key_hash)
        VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''))
        RETURNING id, serial, COALESCE(label,''), COALESCE(vehicle_id,''),
                  api_key_hash IS NOT NULL, is_active, last_seen, COALESCE(last_ip,''), created_at`
	var d Device
	err := s.op().QueryRow(ctx, q, in.Serial, in.Label, in.VehicleID, keyHash).Scan(
		&d.ID, &d.Serial, &d.Label, &d.VehicleID,
		&d.HasAPIKey, &d.IsActive, &d.LastSeen, &d.LastIP, &d.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &CreatedDevice{Device: d, APIKeyPlaintext: plaintext}, nil
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	const q = `
        SELECT id, serial, COALESCE(label,''), COALESCE(vehicle_id,''),
               api_key_hash IS NOT NULL, is_active, last_seen, COALESCE(last_ip,''), created_at
        FROM iot_devices ORDER BY created_at DESC`
	rows, err := s.op().Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(
			&d.ID, &d.Serial, &d.Label, &d.VehicleID,
			&d.HasAPIKey, &d.IsActive, &d.LastSeen, &d.LastIP, &d.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDevice(ctx context.Context, id int64) (*Device, error) {
	const q = `
        SELECT id, serial, COALESCE(label,''), COALESCE(vehicle_id,''),
               api_key_hash IS NOT NULL, is_active, last_seen, COALESCE(last_ip,''), created_at
        FROM iot_devices WHERE id = $1`
	var d Device
	err := s.op().QueryRow(ctx, q, id).Scan(
		&d.ID, &d.Serial, &d.Label, &d.VehicleID,
		&d.HasAPIKey, &d.IsActive, &d.LastSeen, &d.LastIP, &d.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	return &d, err
}

// FindBySerial is what the TCP gateway uses to authenticate an incoming
// connection by IMEI. Returns ErrDeviceNotFound if the serial is unknown,
// ErrInactiveDevice if the device is registered but disabled.
func (s *Store) FindBySerial(ctx context.Context, serial string) (*Device, error) {
	const q = `
        SELECT id, serial, COALESCE(label,''), COALESCE(vehicle_id,''),
               api_key_hash IS NOT NULL, is_active, last_seen, COALESCE(last_ip,''), created_at
        FROM iot_devices WHERE serial = $1`
	var d Device
	err := s.op().QueryRow(ctx, q, serial).Scan(
		&d.ID, &d.Serial, &d.Label, &d.VehicleID,
		&d.HasAPIKey, &d.IsActive, &d.LastSeen, &d.LastIP, &d.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	if !d.IsActive {
		return nil, ErrInactiveDevice
	}
	return &d, nil
}

// AuthenticateAPIKey resolves a device by the plaintext API key supplied in
// the HTTP Authorization header. Constant-time compare via the index lookup
// on api_key_hash; we hash the supplied key and look it up directly.
func (s *Store) AuthenticateAPIKey(ctx context.Context, plaintext string) (*Device, error) {
	if plaintext == "" {
		return nil, ErrInvalidAPIKey
	}
	digest := hashAPIKey(plaintext)
	const q = `
        SELECT id, serial, COALESCE(label,''), COALESCE(vehicle_id,''),
               api_key_hash IS NOT NULL, is_active, last_seen, COALESCE(last_ip,''), created_at
        FROM iot_devices WHERE api_key_hash = $1`
	var d Device
	err := s.op().QueryRow(ctx, q, digest).Scan(
		&d.ID, &d.Serial, &d.Label, &d.VehicleID,
		&d.HasAPIKey, &d.IsActive, &d.LastSeen, &d.LastIP, &d.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidAPIKey
	}
	if err != nil {
		return nil, err
	}
	if !d.IsActive {
		return nil, ErrInactiveDevice
	}
	return &d, nil
}

type UpdateDeviceInput struct {
	// Nil pointer = leave column unchanged (PATCH semantics).
	// VehicleID non-nil: set vehicle_id to the string, or NULL when *VehicleID == "".
	Label     *string
	VehicleID *string
	IsActive  *bool
}

func (s *Store) UpdateDevice(ctx context.Context, id int64, in UpdateDeviceInput) (*Device, error) {
	labelSet := in.Label != nil
	labelVal := ""
	if in.Label != nil {
		labelVal = *in.Label
	}
	vehicleSet := in.VehicleID != nil
	vehicleVal := ""
	if in.VehicleID != nil {
		vehicleVal = *in.VehicleID
	}
	activeSet := in.IsActive != nil
	activeVal := false
	if in.IsActive != nil {
		activeVal = *in.IsActive
	}
	const q = `
        UPDATE iot_devices SET
            label      = CASE WHEN $2 THEN $3::text ELSE label END,
            vehicle_id = CASE WHEN $4 THEN NULLIF($5::text, '') ELSE vehicle_id END,
            is_active  = CASE WHEN $6 THEN $7::bool ELSE is_active END
        WHERE id = $1
        RETURNING id, serial, COALESCE(label,''), COALESCE(vehicle_id,''),
                  api_key_hash IS NOT NULL, is_active, last_seen, COALESCE(last_ip,''), created_at`
	var d Device
	err := s.op().QueryRow(ctx, q, id, labelSet, labelVal, vehicleSet, vehicleVal, activeSet, activeVal).Scan(
		&d.ID, &d.Serial, &d.Label, &d.VehicleID,
		&d.HasAPIKey, &d.IsActive, &d.LastSeen, &d.LastIP, &d.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	return &d, err
}

func (s *Store) DeleteDevice(ctx context.Context, id int64) error {
	tag, err := s.op().Exec(ctx, `DELETE FROM iot_devices WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// RotateAPIKey issues a fresh plaintext key and updates the stored digest.
// The plaintext is shown to the caller exactly once.
func (s *Store) RotateAPIKey(ctx context.Context, id int64) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	plaintext := base64.RawURLEncoding.EncodeToString(buf)
	tag, err := s.op().Exec(ctx,
		`UPDATE iot_devices SET api_key_hash = $1 WHERE id = $2`,
		hashAPIKey(plaintext), id)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrDeviceNotFound
	}
	return plaintext, nil
}

func (s *Store) MarkSeen(ctx context.Context, deviceID int64, ip string) error {
	_, err := s.op().Exec(ctx,
		`UPDATE iot_devices SET last_seen = NOW(), last_ip = NULLIF($2, '') WHERE id = $1`,
		deviceID, ip,
	)
	return err
}

// ──────────────────────────────── Pings ────────────────────────────────

// InsertPings persists a batch. Duplicate (vehicle_id, ts) rows are skipped
// when migration 0015 unique index is present.
func (s *Store) InsertPings(ctx context.Context, pings []Ping) (int, error) {
	if len(pings) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	for _, p := range pings {
		raw := p.Raw
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		batch.Queue(sqlInsertPing,
			p.VehicleID, p.DeviceID, p.TS, p.Lat, p.Lng,
			p.Altitude, p.Heading, p.SpeedKmh, p.Satellites,
			p.Odo, p.FuelLevel, p.Ignition, p.EventID, raw,
		)
	}
	br := s.tel().SendBatch(ctx, batch)
	defer br.Close()
	inserted := 0
	for range pings {
		tag, err := br.Exec()
		if err != nil {
			return inserted, fmt.Errorf("insert ping: %w", err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// ListDailySummaries returns telemetry_daily rows for one vehicle in [from, to] (UTC dates).
func (s *Store) ListDailySummaries(ctx context.Context, vehicleID string, from, to time.Time) ([]DailySummary, error) {
	rows, err := s.tel().Query(ctx, sqlListDaily, vehicleID, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailySummary
	for rows.Next() {
		var sum DailySummary
		if err := rows.Scan(
			&sum.VehicleID, &sum.Day, &sum.PingCount, &sum.DistanceKm,
			&sum.MaxSpeedKmh, &sum.AvgSpeedKmh, &sum.FuelUsedLitres,
			&sum.MovingMinutes, &sum.IdleMinutes, &sum.FirstPing, &sum.LastPing,
		); err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// LatestPing returns the most recent ping for a vehicle, or nil if none.
func (s *Store) LatestPing(ctx context.Context, vehicleID string) (*Ping, error) {
	row := s.tel().QueryRow(ctx, sqlLatestPing, vehicleID)
	p, err := scanPing(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// Track returns pings for a vehicle in [from, to], oldest first, capped at
// limit (default 5000). When after is non-nil, only pings with ts > after are returned (cursor pagination).
func (s *Store) Track(ctx context.Context, vehicleID string, from, to time.Time, limit int, after *time.Time) ([]Ping, error) {
	maxRows := MaxTrackRowLimit()
	if limit <= 0 {
		limit = 5000
	}
	if limit > maxRows {
		limit = maxRows
	}
	var rows pgx.Rows
	var err error
	if after != nil && !after.IsZero() {
		rows, err = s.tel().Query(ctx, sqlTrackPingsAfter, vehicleID, *after, from, to, limit)
	} else {
		rows, err = s.tel().Query(ctx, sqlTrackPings, vehicleID, from, to, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ping
	for rows.Next() {
		p, err := scanPing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// PingsForDay loads every ping for one (vehicle, day) UTC, oldest first.
// Used by cmd/telemetry-aggregate. Returns lat/lng/speed/fuel/ignition only —
// the aggregator doesn't read raw, altitude, etc.
func (s *Store) PingsForDay(ctx context.Context, vehicleID string, day time.Time) ([]Ping, error) {
	start := day.UTC().Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)
	rows, err := s.tel().Query(ctx, sqlPingsForDay, vehicleID, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ping
	for rows.Next() {
		var p Ping
		p.VehicleID = vehicleID
		if err := rows.Scan(&p.TS, &p.Lat, &p.Lng, &p.SpeedKmh, &p.FuelLevel, &p.Ignition); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DistinctVehicleDays returns every (vehicle_id, day) pair that has at least
// one ping in [from, to). Days are returned at UTC midnight. Used to drive
// the aggregator without scanning vehicles that have no telemetry.
func (s *Store) DistinctVehicleDays(ctx context.Context, from, to time.Time) ([]struct {
	VehicleID string
	Day       time.Time
}, error) {
	rows, err := s.tel().Query(ctx, sqlDistinctVehicleDays, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		VehicleID string
		Day       time.Time
	}
	for rows.Next() {
		var v struct {
			VehicleID string
			Day       time.Time
		}
		if err := rows.Scan(&v.VehicleID, &v.Day); err != nil {
			return nil, err
		}
		v.Day = v.Day.UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpsertDaily inserts or replaces one telemetry_daily row. Idempotent — the
// aggregator can re-run the same day without producing duplicates.
func (s *Store) UpsertDaily(ctx context.Context, sum DailySummary) error {
	const q = `
        INSERT INTO telemetry_daily (
            vehicle_id, day, ping_count, distance_km, max_speed_kmh, avg_speed_kmh,
            fuel_used_litres, moving_minutes, idle_minutes, first_ping, last_ping)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
        ON CONFLICT (vehicle_id, day) DO UPDATE SET
            ping_count       = EXCLUDED.ping_count,
            distance_km      = EXCLUDED.distance_km,
            max_speed_kmh    = EXCLUDED.max_speed_kmh,
            avg_speed_kmh    = EXCLUDED.avg_speed_kmh,
            fuel_used_litres = EXCLUDED.fuel_used_litres,
            moving_minutes   = EXCLUDED.moving_minutes,
            idle_minutes     = EXCLUDED.idle_minutes,
            first_ping       = EXCLUDED.first_ping,
            last_ping        = EXCLUDED.last_ping`
	_, err := s.tel().Exec(ctx, q,
		sum.VehicleID, sum.Day, sum.PingCount, sum.DistanceKm,
		sum.MaxSpeedKmh, sum.AvgSpeedKmh, sum.FuelUsedLitres,
		sum.MovingMinutes, sum.IdleMinutes, sum.FirstPing, sum.LastPing,
	)
	return err
}

// VehicleTankCapacities returns id → tank_capacity_litres for the given
// set of vehicle ids. Vehicles without a recorded capacity are absent
// from the map. Used by the aggregator to convert fuel_level (%) to litres.
func (s *Store) VehicleTankCapacities(ctx context.Context, vehicleIDs []string) (map[string]int, error) {
	if len(vehicleIDs) == 0 {
		return map[string]int{}, nil
	}
	const q = `
        SELECT id, tank_capacity_litres
        FROM vehicles
        WHERE id = ANY($1) AND tank_capacity_litres IS NOT NULL`
	rows, err := s.op().Query(ctx, q, vehicleIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int, len(vehicleIDs))
	for rows.Next() {
		var id string
		var cap int
		if err := rows.Scan(&id, &cap); err != nil {
			return nil, err
		}
		out[id] = cap
	}
	return out, rows.Err()
}

// InsertFuelEvents persists a batch. Conflicts on (vehicle_id, ts, kind)
// are ignored — re-running the aggregator over the same day produces the
// same events and we treat them as duplicates.
func (s *Store) InsertFuelEvents(ctx context.Context, events []FuelEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := s.tel().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	const q = `
        INSERT INTO fuel_events (
            vehicle_id, kind, ts, delta_pct, delta_litres,
            before_pct, after_pct, odo, speed_kmh, ignition,
            confidence, notes)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
        ON CONFLICT (vehicle_id, ts, kind) DO NOTHING`
	written := 0
	for _, ev := range events {
		tag, err := tx.Exec(ctx, q,
			ev.VehicleID, ev.Kind, ev.TS, ev.DeltaPct, ev.DeltaLitres,
			ev.BeforePct, ev.AfterPct, ev.Odo, ev.SpeedKmh, ev.Ignition,
			ev.Confidence, ev.Notes,
		)
		if err != nil {
			return written, err
		}
		written += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return written, err
	}
	return written, nil
}

// FuelHistory returns the fuel_level series for one vehicle in [from, to)
// — separate from refuel transactions in fuel_records. Empty readings
// are skipped (NULL in fuel_level).
func (s *Store) FuelHistory(ctx context.Context, vehicleID string, from, to time.Time, limit int) ([]Ping, error) {
	if limit <= 0 || limit > 50000 {
		limit = 5000
	}
	rows, err := s.tel().Query(ctx, sqlFuelHistory, vehicleID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ping
	for rows.Next() {
		var p Ping
		p.VehicleID = vehicleID
		if err := rows.Scan(&p.TS, &p.Lat, &p.Lng, &p.SpeedKmh, &p.FuelLevel, &p.Ignition, &p.Odo); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FuelEvents returns auto-detected events for one vehicle. kind is optional
// ("refuel" | "drop" | ""), confidence is optional ("high" | "medium" | "low" | "").
func (s *Store) FuelEvents(ctx context.Context, vehicleID string, from, to time.Time, kind, confidence string, limit int) ([]FuelEvent, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	const q = `
        SELECT id, vehicle_id, kind, ts, delta_pct, delta_litres,
               before_pct, after_pct, odo, speed_kmh, ignition,
               confidence, COALESCE(notes,''), COALESCE(fuel_record_id,'')
        FROM fuel_events
        WHERE vehicle_id = $1 AND ts BETWEEN $2 AND $3
          AND ($4 = '' OR kind = $4)
          AND ($5 = '' OR confidence = $5)
        ORDER BY ts DESC LIMIT $6`
	rows, err := s.tel().Query(ctx, q, vehicleID, from, to, kind, confidence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FuelEvent
	for rows.Next() {
		var ev FuelEvent
		if err := rows.Scan(
			&ev.ID, &ev.VehicleID, &ev.Kind, &ev.TS, &ev.DeltaPct, &ev.DeltaLitres,
			&ev.BeforePct, &ev.AfterPct, &ev.Odo, &ev.SpeedKmh, &ev.Ignition,
			&ev.Confidence, &ev.Notes, &ev.FuelRecordID,
		); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// FleetFuelAnomalies returns recent drops + low-confidence refuels across
// the whole fleet, oldest excluded — sorted by ts desc. Used by the
// dashboard's "fuel anomaly" hot-list.
func (s *Store) FleetFuelAnomalies(ctx context.Context, from, to time.Time, limit int) ([]FuelEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	const q = `
        SELECT id, vehicle_id, kind, ts, delta_pct, delta_litres,
               before_pct, after_pct, odo, speed_kmh, ignition,
               confidence, COALESCE(notes,''), COALESCE(fuel_record_id,'')
        FROM fuel_events
        WHERE ts BETWEEN $1 AND $2
          AND (kind = 'drop' OR (kind = 'refuel' AND confidence = 'low'))
        ORDER BY ts DESC LIMIT $3`
	rows, err := s.tel().Query(ctx, q, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FuelEvent
	for rows.Next() {
		var ev FuelEvent
		if err := rows.Scan(
			&ev.ID, &ev.VehicleID, &ev.Kind, &ev.TS, &ev.DeltaPct, &ev.DeltaLitres,
			&ev.BeforePct, &ev.AfterPct, &ev.Odo, &ev.SpeedKmh, &ev.Ignition,
			&ev.Confidence, &ev.Notes, &ev.FuelRecordID,
		); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// FuelSummary aggregates daily rows for one vehicle in [from, to]. Used
// by the per-vehicle fuel-summary endpoint to give distance, fuel used,
// efficiency in km/L, and event counts at a glance.
type FuelSummary struct {
	VehicleID       string    `json:"vehicleId"`
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	DistanceKm      float64   `json:"distanceKm"`
	FuelUsedLitres  *float64  `json:"fuelUsedLitres,omitempty"`
	KmPerLitre      *float64  `json:"kmPerLitre,omitempty"`
	RefuelCount     int       `json:"refuelCount"`
	DropCount       int       `json:"dropCount"`
	RefuelLitres    *float64  `json:"refuelLitres,omitempty"`
	DropLitres      *float64  `json:"dropLitres,omitempty"`
}

func (s *Store) FuelSummary(ctx context.Context, vehicleID string, from, to time.Time) (*FuelSummary, error) {
	out := &FuelSummary{VehicleID: vehicleID, From: from, To: to}

	// Daily rollup totals
	const q1 = `
        SELECT COALESCE(SUM(distance_km), 0),
               SUM(fuel_used_litres)
        FROM telemetry_daily
        WHERE vehicle_id = $1 AND day >= $2::date AND day <= $3::date`
	var fuelUsed *float64
	if err := s.tel().QueryRow(ctx, q1, vehicleID, from, to).Scan(&out.DistanceKm, &fuelUsed); err != nil {
		return nil, err
	}
	out.FuelUsedLitres = fuelUsed
	if fuelUsed != nil && *fuelUsed > 0 && out.DistanceKm > 0 {
		eff := out.DistanceKm / *fuelUsed
		out.KmPerLitre = &eff
	}

	// Event counts and litre totals
	const q2 = `
        SELECT
            COALESCE(SUM(CASE WHEN kind='refuel' THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN kind='drop'   THEN 1 ELSE 0 END), 0),
            SUM(CASE WHEN kind='refuel' THEN delta_litres END),
            SUM(CASE WHEN kind='drop'   THEN delta_litres END)
        FROM fuel_events
        WHERE vehicle_id = $1 AND ts BETWEEN $2 AND $3`
	var refLitres, dropLitres *float64
	if err := s.tel().QueryRow(ctx, q2, vehicleID, from, to).Scan(
		&out.RefuelCount, &out.DropCount, &refLitres, &dropLitres,
	); err != nil {
		return nil, err
	}
	out.RefuelLitres = refLitres
	out.DropLitres = dropLitres
	return out, nil
}

// PurgeBefore drops pings older than the cutoff. Called by cmd/telemetry-purge.
func (s *Store) PurgeBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.tel().Exec(ctx, sqlPurgeBefore, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ──────────────────────────────── helpers ────────────────────────────────

func hashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plaintext)))
	return hex.EncodeToString(sum[:])
}

type rowScanner interface {
	Scan(...any) error
}

func scanPing(row rowScanner) (*Ping, error) {
	var p Ping
	var raw []byte
	err := row.Scan(
		&p.VehicleID, &p.DeviceID, &p.TS, &p.Lat, &p.Lng,
		&p.Altitude, &p.Heading, &p.SpeedKmh, &p.Satellites,
		&p.Odo, &p.FuelLevel, &p.Ignition, &p.EventID, &raw,
	)
	if err != nil {
		return nil, err
	}
	p.Raw = json.RawMessage(raw)
	return &p, nil
}
