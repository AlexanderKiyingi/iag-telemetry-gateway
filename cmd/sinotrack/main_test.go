package main

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/iag/fleet-iot/iot"
)

// fakeStore records the gateway's calls and mimics FindBySerial's active/known
// checks, so the connection loop can be exercised without Postgres.
type fakeStore struct {
	mu       sync.Mutex
	devices  map[string]*iot.Device // serial -> row
	inserted []iot.Ping
	hotState []iot.Ping
	geofence int
	markSeen int
}

func (f *fakeStore) FindBySerial(_ context.Context, serial string) (*iot.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[serial]
	if !ok {
		return nil, iot.ErrDeviceNotFound
	}
	if !d.IsActive {
		return nil, iot.ErrInactiveDevice
	}
	return d, nil
}

func (f *fakeStore) MarkSeen(_ context.Context, _ int64, _ string) error {
	f.mu.Lock()
	f.markSeen++
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) InsertPings(_ context.Context, pings []iot.Ping) (int, error) {
	f.mu.Lock()
	f.inserted = append(f.inserted, pings...)
	f.mu.Unlock()
	return len(pings), nil
}

func (f *fakeStore) ApplyVehicleHotState(_ context.Context, p iot.Ping) (iot.StatusSyncResult, error) {
	f.mu.Lock()
	f.hotState = append(f.hotState, p)
	f.mu.Unlock()
	return iot.StatusSyncResult{}, nil
}

func (f *fakeStore) ApplyGeofenceTransitions(_ context.Context, _ []iot.GeofenceTransition) error {
	f.mu.Lock()
	f.geofence++
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) counts() (insert, hot, mark int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inserted), len(f.hotState), f.markSeen
}

// runHandle feeds frames through one gateway connection and returns once the
// loop exits (peer close). The hub is nil, exercising the nil-guard.
func runHandle(t *testing.T, store hqStore, frames ...string) {
	t.Helper()
	srv, cli := net.Pipe()
	g := &hqGateway{store: store, sem: make(chan struct{}, 1)}

	done := make(chan struct{})
	go func() {
		g.handle(srv)
		close(done)
	}()

	for _, f := range frames {
		if _, err := io.WriteString(cli, f); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	_ = cli.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handle did not return after peer close")
	}
}

const (
	// Same device id in fields[1]; differ only by which iot_devices row backs it.
	frameBound   = "*HQ,9170503816,V1,123506,A,2232.6024,N,11355.7983,E,012.30,090,131216,FFFFFBFF#"
	frameUnbound = "*HQ,9170000002,V1,123506,A,2232.6024,N,11355.7983,E,000.00,000,131216,FFFFFBFF#"
	frameNoFix   = "*HQ,9170503816,V1,123506,V,0000.0000,N,00000.0000,E,000.00,000,131216,FFFFFBFF#"
	frameUnknown = "*HQ,9999999999,V1,123506,A,2232.6024,N,11355.7983,E,000.00,000,131216,FFFFFBFF#"
)

func storeWith(devs ...*iot.Device) *fakeStore {
	m := make(map[string]*iot.Device, len(devs))
	for _, d := range devs {
		m[d.Serial] = d
	}
	return &fakeStore{devices: m}
}

func TestHandleBoundDeviceValidFixIngests(t *testing.T) {
	s := storeWith(&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true})
	runHandle(t, s, frameBound)

	insert, hot, mark := s.counts()
	if insert != 1 {
		t.Fatalf("expected 1 ping inserted, got %d", insert)
	}
	if hot != 1 {
		t.Fatalf("expected hot-state sync once, got %d", hot)
	}
	if mark == 0 {
		t.Fatalf("expected device marked seen")
	}
	if s.geofence == 0 {
		t.Fatalf("expected geofence transitions evaluated")
	}
	// Sanity on the decoded ping that reached the store.
	p := s.inserted[0]
	if p.VehicleID != "VEH-001" {
		t.Errorf("ping vehicle = %q, want VEH-001", p.VehicleID)
	}
	if p.SpeedKmh == nil || *p.SpeedKmh < 22.7 || *p.SpeedKmh > 22.9 {
		t.Errorf("speed = %v, want ~22.78 km/h (12.30 kn)", p.SpeedKmh)
	}
}

func TestHandleUnboundDeviceSkipsInsert(t *testing.T) {
	s := storeWith(&iot.Device{ID: 2, Serial: "9170000002", VehicleID: "", IsActive: true})
	runHandle(t, s, frameUnbound)

	insert, hot, mark := s.counts()
	if insert != 0 {
		t.Fatalf("unbound device must not write pings, got %d", insert)
	}
	if hot != 0 {
		t.Fatalf("unbound device must not sync hot-state, got %d", hot)
	}
	if mark == 0 {
		t.Fatalf("unbound device should still be marked seen")
	}
}

func TestHandleInvalidFixSkipsInsert(t *testing.T) {
	s := storeWith(&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true})
	runHandle(t, s, frameNoFix)

	insert, _, mark := s.counts()
	if insert != 0 {
		t.Fatalf("no-fix frame must not write pings, got %d", insert)
	}
	if mark == 0 {
		t.Fatalf("expected device marked seen on no-fix frame")
	}
}

func TestHandleUnknownDeviceClosesWithoutIngest(t *testing.T) {
	s := storeWith(&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true})
	runHandle(t, s, frameUnknown)

	insert, _, mark := s.counts()
	if insert != 0 || mark != 0 {
		t.Fatalf("unknown device must be rejected: insert=%d mark=%d", insert, mark)
	}
}

func TestHandlePinsToFirstDevice(t *testing.T) {
	// After auth on the bound device, a frame bearing a different id is dropped:
	// one socket cannot inject positions for another device/vehicle.
	s := storeWith(
		&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true},
		&iot.Device{ID: 2, Serial: "9170000002", VehicleID: "VEH-002", IsActive: true},
	)
	runHandle(t, s, frameBound, frameUnbound) // 2nd frame's id != bound id

	insert, _, _ := s.counts()
	if insert != 1 {
		t.Fatalf("expected only the bound device's frame to ingest, got %d", insert)
	}
}

func TestMessageToPingMapsFields(t *testing.T) {
	msg, err := iot.ParseHQFrame(frameBound)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	dev := &iot.Device{ID: 7, Serial: "9170503816", VehicleID: "VEH-001"}
	p := messageToPing(msg, dev)

	if p.VehicleID != "VEH-001" {
		t.Errorf("vehicle = %q", p.VehicleID)
	}
	if p.DeviceID == nil || *p.DeviceID != 7 {
		t.Errorf("deviceID = %v, want 7", p.DeviceID)
	}
	if p.Lat < 22.54 || p.Lat > 22.55 {
		t.Errorf("lat = %v, want ~22.5434", p.Lat)
	}
	// ST-901 carries no fuel/odo/ignition — these must stay nil, not zero values.
	if p.FuelLevel != nil || p.Odo != nil || p.Ignition != nil {
		t.Errorf("expected fuel/odo/ignition nil, got %v/%v/%v", p.FuelLevel, p.Odo, p.Ignition)
	}
	// The HQ status word rides in Raw, not decoded into typed fields.
	if len(p.Raw) == 0 {
		t.Errorf("expected raw HQ extras to be preserved")
	}
}
