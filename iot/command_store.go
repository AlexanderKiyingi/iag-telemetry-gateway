package iot

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeviceCommand is one queued or completed server→device command.
type DeviceCommand struct {
	ID          int64
	DeviceID    int64
	VehicleID   string
	Kind        string
	Status      string
	RequestedBy string
	RequestedAt time.Time
	ExpiresAt   time.Time
}

// EnqueueCommand queues a command for a device.
//
// A partial unique index allows only one pending command per device, so this
// returns ErrCommandPending rather than stacking three immobilises that would
// all fire the moment the truck reconnects.
func (s *Store) EnqueueCommand(ctx context.Context, deviceID int64, vehicleID, kind, requestedBy string, ttl time.Duration) (*DeviceCommand, error) {
	if kind != CommandImmobilize && kind != CommandMobilize {
		return nil, errors.New("command: unsupported kind " + kind)
	}
	if ttl <= 0 {
		ttl = DefaultInterlockConfig().MaxQueueAge
	}
	var c DeviceCommand
	err := s.op().QueryRow(ctx, `
		INSERT INTO device_commands (device_id, vehicle_id, kind, requested_by, expires_at)
		VALUES ($1, NULLIF($2,''), $3, $4, NOW() + $5::interval)
		RETURNING id, device_id, COALESCE(vehicle_id,''), kind, status, requested_by, requested_at, expires_at`,
		deviceID, vehicleID, kind, requestedBy, ttl.String(),
	).Scan(&c.ID, &c.DeviceID, &c.VehicleID, &c.Kind, &c.Status, &c.RequestedBy, &c.RequestedAt, &c.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ErrCommandPending is returned when a device already has a queued command.
var ErrCommandPending = errors.New("command: this device already has a pending command")

// NextPendingCommand returns the queued command for a device, if any.
func (s *Store) NextPendingCommand(ctx context.Context, deviceID int64) (*DeviceCommand, error) {
	var c DeviceCommand
	err := s.op().QueryRow(ctx, `
		SELECT id, device_id, COALESCE(vehicle_id,''), kind, status, requested_by, requested_at, expires_at
		  FROM device_commands
		 WHERE device_id = $1 AND status = 'pending'
		 ORDER BY requested_at
		 LIMIT 1`, deviceID,
	).Scan(&c.ID, &c.DeviceID, &c.VehicleID, &c.Kind, &c.Status, &c.RequestedBy, &c.RequestedAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// MarkCommandSent records a successful delivery, including the exact bytes.
func (s *Store) MarkCommandSent(ctx context.Context, id int64, payload string, snap VehicleSnapshot) error {
	_, err := s.op().Exec(ctx, `
		UPDATE device_commands
		   SET status = 'sent', sent_at = NOW(), decided_at = NOW(),
		       payload = $2,
		       decision_speed_kmh = $3,
		       decision_fix_age_s = $4
		 WHERE id = $1`,
		id, payload, snap.SpeedKmh, int(snap.FixAge.Seconds()))
	return err
}

// MarkCommandRefused records a refusal and why.
//
// Refusals are retained deliberately: "why did nothing happen when I pressed
// the button" and "who tried to stop this truck" are both questions an incident
// review has to answer, and a deleted row answers neither.
func (s *Store) MarkCommandRefused(ctx context.Context, id int64, reason string, snap VehicleSnapshot) error {
	status := CommandRefused
	_, err := s.op().Exec(ctx, `
		UPDATE device_commands
		   SET status = $2, decided_at = NOW(), refused_reason = $3,
		       decision_speed_kmh = $4,
		       decision_fix_age_s = $5
		 WHERE id = $1`,
		id, status, reason, snap.SpeedKmh, int(snap.FixAge.Seconds()))
	return err
}

// VehicleSnapshotFor reads the state the interlock will judge, from the
// vehicle's most recent ping. Deliberately taken at delivery time.
func (s *Store) VehicleSnapshotFor(ctx context.Context, vehicleID string) (VehicleSnapshot, error) {
	if vehicleID == "" {
		return VehicleSnapshot{}, nil
	}
	var speed *float64
	var ts time.Time
	err := s.tel().QueryRow(ctx, `
		SELECT speed_kmh, ts FROM telemetry_timeseries
		 WHERE vehicle_id = $1
		 ORDER BY ts DESC
		 LIMIT 1`, vehicleID,
	).Scan(&speed, &ts)
	if errors.Is(err, pgx.ErrNoRows) {
		// No telemetry at all: HasFix stays false and the interlock refuses to
		// immobilise blind.
		return VehicleSnapshot{}, nil
	}
	if err != nil {
		return VehicleSnapshot{}, err
	}
	return VehicleSnapshot{SpeedKmh: speed, FixAge: time.Since(ts), HasFix: true}, nil
}
