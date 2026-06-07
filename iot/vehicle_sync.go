package iot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StatusSyncResult is returned when SyncVehicleFromPing updates a registry row.
type StatusSyncResult struct {
	VehicleID      string
	Changed        bool
	PreviousStatus string
	NewStatus      string
}

const (
	fleetEventSource             = "iag.fleet"
	fleetEventSpecVersion        = "1.0"
	typeVehicleStatusChanged     = "fleet.vehicle.status_changed"
)

// SyncVehicleFromPing pushes a ping's position/speed into the vehicles row
// so the existing API surface keeps reflecting the latest known state.
// Best-effort: vehicle not found yields an empty result without error.
func (s *Store) SyncVehicleFromPing(ctx context.Context, p Ping) (StatusSyncResult, error) {
	speed := 0.0
	if p.SpeedKmh != nil {
		speed = *p.SpeedKmh
	}
	const q = `
        WITH prev AS (
            SELECT status FROM vehicles WHERE id = $1
        ),
        upd AS (
            UPDATE vehicles SET
                lat       = $2,
                lng       = $3,
                heading   = COALESCE($4, heading),
                speed     = COALESCE($5, speed),
                fuel      = COALESCE($6, fuel),
                odo       = COALESCE($7, odo),
                last_seen = $8,
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
	err := s.op().QueryRow(ctx, q,
		p.VehicleID, p.Lat, p.Lng, p.Heading, p.SpeedKmh, p.FuelLevel, p.Odo, p.TS,
		speed, movingThresholdKmh,
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
	if !result.Changed || result.VehicleID == "" {
		return nil
	}
	return s.enqueueFleetEvent(ctx, typeVehicleStatusChanged, result.VehicleID, map[string]any{
		"vehicleId":      result.VehicleID,
		"status":         result.NewStatus,
		"previousStatus": result.PreviousStatus,
		"source":         "telemetry",
	})
}

// PublishStatusChanges batch-enqueues stale-offline transitions.
func (s *Store) PublishStatusChanges(ctx context.Context, changes []StatusChange) error {
	for _, ch := range changes {
		if err := s.enqueueFleetEvent(ctx, typeVehicleStatusChanged, ch.VehicleID, map[string]any{
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

func (s *Store) enqueueFleetEvent(ctx context.Context, eventType, key string, data map[string]any) error {
	if !eventBusEnabled() {
		return nil
	}
	evt := map[string]any{
		"id":            uuid.NewString(),
		"type":          eventType,
		"time":          time.Now().UTC().Format(time.RFC3339Nano),
		"source":        fleetEventSource,
		"specversion":   fleetEventSpecVersion,
		"data":          data,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	var keyArg any
	if key != "" {
		keyArg = key
	}
	_, err = s.op().Exec(ctx, `
		INSERT INTO fleet_event_outbox (event_type, event_key, payload)
		VALUES ($1, $2, $3::jsonb)
	`, eventType, keyArg, body)
	return err
}

func eventBusEnabled() bool {
	return strings.EqualFold(os.Getenv("EVENT_BUS_ENABLED"), "true")
}
