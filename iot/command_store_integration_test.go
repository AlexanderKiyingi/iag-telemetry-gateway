package iot

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The gateway's control flow is covered with a fake store, but that proves
// nothing about the SQL underneath: the partial unique index, the interval
// cast, the CHECK constraints and the NULLIF handling are all only exercised by
// a real database. Immobilisation is not a feature to ship on unverified SQL.
//
//	TEST_DATABASE_URL=postgres://... go test ./iot/... -run Integration_Command
func commandTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping command store integration tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM device_commands`); err != nil {
		t.Fatalf("clean device_commands: %v", err)
	}
	return pool
}

// seedDeviceAndVehicle creates the rows the FKs require and returns the device id.
func seedDeviceAndVehicle(t *testing.T, pool *pgxpool.Pool, serial, vehicleID string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO vehicles (id, plate, type, make, model, year, vehicle_class, ownership,
		                      status, location, lat, lng, capacity, last_seen, mech_status)
		VALUES ($1, $2, 'truck','Isuzu','FRR',2021,'heavy','Owned','idle','Yard',0.3,32.5,'8t',NOW(),'operational')
		ON CONFLICT (id) DO NOTHING`, vehicleID, "PL-"+vehicleID); err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO iot_devices (serial, label, vehicle_id, is_active, model)
		VALUES ($1, 'cmd test', $2, true, 'ST-901')
		ON CONFLICT (serial) DO UPDATE SET vehicle_id = EXCLUDED.vehicle_id
		RETURNING id`, serial, vehicleID).Scan(&id); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	return id
}

func TestIntegration_CommandEnqueueAndRead(t *testing.T) {
	pool := commandTestPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	devID := seedDeviceAndVehicle(t, pool, "CMD-0001", "VEH-CMD1")

	cmd, err := s.EnqueueCommand(ctx, devID, "VEH-CMD1", CommandImmobilize, "operator@iag", 15*time.Minute)
	if err != nil {
		t.Fatalf("EnqueueCommand: %v", err)
	}
	if cmd.Status != CommandPending {
		t.Fatalf("status = %q, want pending", cmd.Status)
	}
	// The interval cast is easy to get wrong and would silently expire
	// everything immediately.
	if ttl := time.Until(cmd.ExpiresAt); ttl < 10*time.Minute || ttl > 20*time.Minute {
		t.Fatalf("expires_at is %v away, want ~15m — check the interval cast", ttl.Round(time.Second))
	}

	got, err := s.NextPendingCommand(ctx, devID)
	if err != nil {
		t.Fatalf("NextPendingCommand: %v", err)
	}
	if got == nil || got.ID != cmd.ID || got.Kind != CommandImmobilize {
		t.Fatalf("got %+v, want the queued immobilise", got)
	}
}

// The partial unique index is what stops three queued immobilises all firing
// when the truck reconnects. If the index is wrong this silently succeeds.
func TestIntegration_CommandOnlyOnePending(t *testing.T) {
	pool := commandTestPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	devID := seedDeviceAndVehicle(t, pool, "CMD-0002", "VEH-CMD2")

	if _, err := s.EnqueueCommand(ctx, devID, "VEH-CMD2", CommandImmobilize, "op", time.Minute); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if _, err := s.EnqueueCommand(ctx, devID, "VEH-CMD2", CommandImmobilize, "op", time.Minute); err == nil {
		t.Fatal("a second pending command for the same device must be rejected by the partial unique index")
	}
}

func TestIntegration_CommandMarkSentAndRefused(t *testing.T) {
	pool := commandTestPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	devID := seedDeviceAndVehicle(t, pool, "CMD-0003", "VEH-CMD3")

	sent, err := s.EnqueueCommand(ctx, devID, "VEH-CMD3", CommandImmobilize, "op", time.Minute)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	zero := 0.0
	snap := VehicleSnapshot{SpeedKmh: &zero, FixAge: 3 * time.Second, HasFix: true}
	if err := s.MarkCommandSent(ctx, sent.ID, "*HQ,X,S20,1#", snap); err != nil {
		t.Fatalf("MarkCommandSent: %v", err)
	}

	var status, payload string
	var speed *float64
	var fixAge *int
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(payload,''), decision_speed_kmh, decision_fix_age_s
		   FROM device_commands WHERE id = $1`, sent.ID,
	).Scan(&status, &payload, &speed, &fixAge); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != CommandSent || payload != "*HQ,X,S20,1#" {
		t.Fatalf("status=%q payload=%q, want sent with the exact bytes recorded", status, payload)
	}
	// The decision state is the audit trail; an incident review needs it.
	if speed == nil || *speed != 0 || fixAge == nil || *fixAge != 3 {
		t.Fatalf("decision state not recorded: speed=%v fixAge=%v", speed, fixAge)
	}

	// Marking sent clears the pending slot, so a new command may be queued.
	refused, err := s.EnqueueCommand(ctx, devID, "VEH-CMD3", CommandImmobilize, "op", time.Minute)
	if err != nil {
		t.Fatalf("enqueue after send: %v", err)
	}
	if err := s.MarkCommandRefused(ctx, refused.ID, "vehicle is moving at 70 km/h", snap); err != nil {
		t.Fatalf("MarkCommandRefused: %v", err)
	}
	var rStatus, reason string
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(refused_reason,'') FROM device_commands WHERE id = $1`, refused.ID,
	).Scan(&rStatus, &reason); err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	if rStatus != CommandRefused || reason == "" {
		t.Fatalf("status=%q reason=%q, want the refusal retained with its reason", rStatus, reason)
	}

	// Both rows survive: refusals are part of the audit trail, not cleanup.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM device_commands WHERE device_id = $1`, devID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("device has %d command rows, want both the sent and the refused retained", n)
	}
}

// The CHECK constraints must reject nonsense that bypassed the Go-side guard.
func TestIntegration_CommandRejectsBadKind(t *testing.T) {
	pool := commandTestPool(t)
	ctx := context.Background()
	devID := seedDeviceAndVehicle(t, pool, "CMD-0004", "VEH-CMD4")

	_, err := pool.Exec(ctx, `
		INSERT INTO device_commands (device_id, kind, requested_by, expires_at)
		VALUES ($1, 'self_destruct', 'op', NOW() + interval '1 minute')`, devID)
	if err == nil {
		t.Fatal("the kind CHECK constraint must reject an unrecognised command kind")
	}
}

// VehicleSnapshotFor drives the interlock, so its behaviour with no telemetry
// matters: it must report no fix, which makes the interlock refuse.
func TestIntegration_VehicleSnapshotWithoutTelemetry(t *testing.T) {
	pool := commandTestPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	seedDeviceAndVehicle(t, pool, "CMD-0005", "VEH-CMD5")

	snap, err := s.VehicleSnapshotFor(ctx, "VEH-CMD5")
	if err != nil {
		t.Fatalf("VehicleSnapshotFor: %v", err)
	}
	if snap.HasFix {
		t.Fatal("a vehicle with no telemetry must report no fix")
	}
	if v := EvaluateInterlock(CommandImmobilize, time.Second, snap, DefaultInterlockConfig()); v.Allowed {
		t.Fatal("the interlock must refuse to immobilise a vehicle it cannot locate")
	}
}
