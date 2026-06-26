package iot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// opConn is the subset of pgx shared by *pgxpool.Pool and pgx.Tx, so the
// registry status update and its outbox enqueue can run either directly on the
// pool or together inside one transaction.
type opConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// StatusSyncResult is returned when SyncVehicleFromPing updates a registry row.
type StatusSyncResult struct {
	VehicleID      string
	Changed        bool
	PreviousStatus string
	NewStatus      string
}

const (
	fleetEventSource         = "iag.fleet"
	fleetEventSpecVersion    = "1.0"
	typeVehicleStatusChanged = "fleet.vehicle.status_changed"
)

// SyncVehicleFromPing pushes a ping's position/speed into the vehicles row
// so the existing API surface keeps reflecting the latest known state.
// Best-effort: vehicle not found yields an empty result without error.
func (s *Store) SyncVehicleFromPing(ctx context.Context, p Ping) (StatusSyncResult, error) {
	return s.syncVehicleFromPing(ctx, s.op(), p)
}

func (s *Store) syncVehicleFromPing(ctx context.Context, db opConn, p Ping) (StatusSyncResult, error) {
	speed := 0.0
	if p.SpeedKmh != nil {
		speed = *p.SpeedKmh
	}
	// A ping with no device is a driver-phone (companion app) fix; a bound
	// device is a hardware tracker. The live map labels the two differently.
	fixSource := "device"
	if p.DeviceID == nil {
		fixSource = "mobile"
	}
	const q = `
        WITH prev AS (
            SELECT status FROM vehicles WHERE id = $1
        ),
        upd AS (
            UPDATE vehicles SET
                lat             = $2,
                lng             = $3,
                heading         = COALESCE($4, heading),
                speed           = COALESCE($5, speed),
                fuel            = COALESCE($6, fuel),
                odo             = COALESCE($7, odo),
                last_seen       = $8,
                last_fix_source = $11,
                status    = CASE
                    WHEN vehicles.status = 'maintenance' THEN vehicles.status
                    WHEN $9 >= $10::float8 THEN 'moving'
                    ELSE 'idle'
                END
            WHERE id = $1
              AND EXISTS (SELECT 1 FROM prev)
            RETURNING status
        )
        SELECT prev.status, upd.status
        FROM prev
        LEFT JOIN upd ON TRUE`
	var prevStatus, newStatus *string
	err := db.QueryRow(ctx, q,
		p.VehicleID, p.Lat, p.Lng, p.Heading, p.SpeedKmh, p.FuelLevel, p.Odo, p.TS,
		speed, movingThresholdKmh, fixSource,
	).Scan(&prevStatus, &newStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusSyncResult{}, nil
	}
	if err != nil {
		return StatusSyncResult{}, err
	}
	if prevStatus == nil || newStatus == nil {
		return StatusSyncResult{}, nil
	}
	res := StatusSyncResult{
		VehicleID:      p.VehicleID,
		PreviousStatus: *prevStatus,
		NewStatus:      *newStatus,
	}
	res.Changed = res.PreviousStatus != res.NewStatus
	return res, nil
}

// ApplyVehicleHotState syncs registry position/status from a ping and enqueues
// the status-change event in the SAME transaction, so the vehicles update and
// the fleet_event_outbox row commit atomically — a status change can never be
// applied to the registry without its outbox event (or vice versa). An enqueue
// failure rolls the whole update back and surfaces as an error to the caller
// (treated as a registry-sync failure; pings are already durably persisted).
func (s *Store) ApplyVehicleHotState(ctx context.Context, p Ping) (StatusSyncResult, error) {
	tx, err := s.op().Begin(ctx)
	if err != nil {
		return StatusSyncResult{}, err
	}
	defer tx.Rollback(ctx)

	syncRes, err := s.syncVehicleFromPing(ctx, tx, p)
	if err != nil {
		return StatusSyncResult{}, err
	}
	if err := s.publishStatusChange(ctx, tx, syncRes); err != nil {
		return StatusSyncResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return StatusSyncResult{}, err
	}
	return syncRes, nil
}

// StatusChange is one vehicle whose registry status was updated.
type StatusChange struct {
	VehicleID      string
	PreviousStatus string
	NewStatus      string
}

// MarkStaleVehiclesOffline sets status=offline when last_seen is older than cutoff
// and the vehicle is not in maintenance. Returns per-vehicle status transitions.
func (s *Store) MarkStaleVehiclesOffline(ctx context.Context, cutoff time.Time) ([]StatusChange, error) {
	const q = `
        WITH targets AS (
            SELECT id, status AS prev_status
            FROM vehicles
            WHERE status IN ('moving', 'idle')
              AND last_seen IS NOT NULL
              AND last_seen < $1
        ),
        upd AS (
            UPDATE vehicles v
            SET status = 'offline'
            FROM targets t
            WHERE v.id = t.id
            RETURNING v.id, t.prev_status
        )
        SELECT id, prev_status FROM upd`
	rows, err := s.op().Query(ctx, q, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatusChange
	for rows.Next() {
		var ch StatusChange
		if err := rows.Scan(&ch.VehicleID, &ch.PreviousStatus); err != nil {
			return nil, err
		}
		ch.NewStatus = "offline"
		out = append(out, ch)
	}
	return out, rows.Err()
}

// PublishStatusChange enqueues fleet.vehicle.status_changed on the operational
// outbox when EVENT_BUS_ENABLED=true. Safe no-op when disabled or unchanged.
func (s *Store) PublishStatusChange(ctx context.Context, result StatusSyncResult) error {
	return s.publishStatusChange(ctx, s.op(), result)
}

func (s *Store) publishStatusChange(ctx context.Context, db opConn, result StatusSyncResult) error {
	if !result.Changed || result.VehicleID == "" {
		return nil
	}
	return s.enqueueFleetEvent(ctx, db, typeVehicleStatusChanged, result.VehicleID, map[string]any{
		"vehicleId":      result.VehicleID,
		"status":         result.NewStatus,
		"previousStatus": result.PreviousStatus,
		"source":         "telemetry",
	})
}

// PublishStatusChanges batch-enqueues stale-offline transitions.
func (s *Store) PublishStatusChanges(ctx context.Context, changes []StatusChange) error {
	for _, ch := range changes {
		if err := s.enqueueFleetEvent(ctx, s.op(), typeVehicleStatusChanged, ch.VehicleID, map[string]any{
			"vehicleId":      ch.VehicleID,
			"status":         ch.NewStatus,
			"previousStatus": ch.PreviousStatus,
			"source":         "stale_job",
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) enqueueFleetEvent(ctx context.Context, db opConn, eventType, key string, data map[string]any) error {
	if !eventBusEnabled() {
		return nil
	}
	body, err := json.Marshal(newPlatformEvent(eventType, data))
	if err != nil {
		return err
	}
	var keyArg any
	if key != "" {
		keyArg = key
	}
	_, err = db.Exec(ctx, `
		INSERT INTO fleet_event_outbox (event_type, event_key, payload)
		VALUES ($1, $2, $3::jsonb)
	`, eventType, keyArg, body)
	return err
}

func eventBusEnabled() bool {
	return strings.EqualFold(os.Getenv("EVENT_BUS_ENABLED"), "true")
}
