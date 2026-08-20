package iot

import (
	"context"
	"testing"
	"time"
)

// pipeFake records which pipeline steps ran.
type pipeFake struct {
	inserted  []Ping
	hotState  int
	geofence  int
	overspeed int
	markSeen  int
}

func (f *pipeFake) InsertPings(_ context.Context, p []Ping) (int, error) {
	f.inserted = append(f.inserted, p...)
	return len(p), nil
}
func (f *pipeFake) ApplyVehicleHotState(_ context.Context, _ Ping) (StatusSyncResult, error) {
	f.hotState++
	return StatusSyncResult{Changed: true, NewStatus: "moving"}, nil
}
func (f *pipeFake) ApplyGeofenceTransitions(_ context.Context, _ []GeofenceTransition) error {
	f.geofence++
	return nil
}
func (f *pipeFake) ApplyOverspeed(_ context.Context, _ Ping, _ OverspeedConfig) error {
	f.overspeed++
	return nil
}
func (f *pipeFake) MarkSeen(_ context.Context, _ int64, _ string) error {
	f.markSeen++
	return nil
}

// This is the regression guard for a bug that happened twice: a feature added
// to one gateway's main.go and forgotten in the other's, so Teltonika vehicles
// silently had no speed monitoring, and later saw a different geofence set than
// SinoTrack vehicles.
//
// Asserting the whole sequence here means any gateway that hands pings to the
// pipeline gets every step by construction. A new step added to Ingest without
// updating this test fails loudly rather than reaching one protocol only.
func TestPipeline_runsEveryStep(t *testing.T) {
	f := &pipeFake{}
	p := &Pipeline{Store: f}
	dev := &Device{ID: 1, VehicleID: "VEH-1"}

	res, err := p.Ingest(context.Background(), dev, []Ping{
		{VehicleID: "VEH-1", TS: time.Unix(1000, 0), Lat: 0.3, Lng: 32.5},
		{VehicleID: "VEH-1", TS: time.Unix(2000, 0), Lat: 0.4, Lng: 32.6},
	}, "1.2.3.4", nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if res.Inserted != 2 {
		t.Errorf("inserted = %d, want 2", res.Inserted)
	}
	if f.hotState != 1 {
		t.Errorf("hot-state applied %d times, want exactly 1 (newest ping only)", f.hotState)
	}
	if f.geofence != 1 {
		t.Errorf("geofence evaluated %d times, want 1", f.geofence)
	}
	if f.overspeed != 1 {
		t.Errorf("overspeed evaluated %d times, want 1 — this is the step Teltonika silently lacked", f.overspeed)
	}
	if f.markSeen != 1 {
		t.Errorf("markSeen called %d times, want 1", f.markSeen)
	}
	if !res.StatusChanged || res.NewStatus != "moving" {
		t.Errorf("status change not reported back: %+v", res)
	}
}

// Hot-state, geofence and overspeed must all key off the NEWEST ping in a
// batch. Teltonika delivers buffered records out of order after a signal gap,
// so using the first would rewind a vehicle to where it was an hour ago.
func TestPipeline_usesNewestPing(t *testing.T) {
	var seen Ping
	f := &pipeFake{}
	p := &Pipeline{Store: &newestCapture{pipeFake: f, got: &seen}}

	_, err := p.Ingest(context.Background(), &Device{ID: 1, VehicleID: "VEH-1"}, []Ping{
		{VehicleID: "VEH-1", TS: time.Unix(3000, 0), Lat: 0.9, Lng: 32.9},
		{VehicleID: "VEH-1", TS: time.Unix(9000, 0), Lat: 0.1, Lng: 32.1}, // newest
		{VehicleID: "VEH-1", TS: time.Unix(5000, 0), Lat: 0.5, Lng: 32.5},
	}, "", nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !seen.TS.Equal(time.Unix(9000, 0)) {
		t.Fatalf("hot-state used ts %v, want the newest (9000)", seen.TS.Unix())
	}
}

type newestCapture struct {
	*pipeFake
	got *Ping
}

func (c *newestCapture) ApplyVehicleHotState(ctx context.Context, p Ping) (StatusSyncResult, error) {
	*c.got = p
	return c.pipeFake.ApplyVehicleHotState(ctx, p)
}

// A failed insert must abort: the caller drops the connection so the device
// retries with its buffer intact. Downstream failures must not, because the
// telemetry is already durable.
func TestPipeline_insertFailureAborts(t *testing.T) {
	p := &Pipeline{Store: failingInsert{pipeFake: &pipeFake{}}}
	_, err := p.Ingest(context.Background(), &Device{ID: 1, VehicleID: "V"},
		[]Ping{{VehicleID: "V", TS: time.Unix(1, 0)}}, "", nil)
	if err == nil {
		t.Fatal("a failed insert must be returned so the gateway drops the connection")
	}
}

type failingInsert struct{ *pipeFake }

func (failingInsert) InsertPings(_ context.Context, _ []Ping) (int, error) {
	return 0, context.DeadlineExceeded
}

func TestPipeline_emptyBatchIsNoOp(t *testing.T) {
	f := &pipeFake{}
	p := &Pipeline{Store: f}
	if _, err := p.Ingest(context.Background(), &Device{ID: 1}, nil, "", nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if len(f.inserted) != 0 || f.hotState != 0 {
		t.Fatal("an empty batch must touch nothing")
	}
}
