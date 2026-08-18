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
// statusObs is one RecordStatusWord call, so tests can assert that the status
// word is captured even for frames the gateway skips.
type statusObs struct {
	deviceID  int64
	frameType string
	word      string
	sample    string
	hadFix    bool
}

type fakeStore struct {
	mu             sync.Mutex
	devices        map[string]*iot.Device // serial -> row
	inserted       []iot.Ping
	hotState       []iot.Ping
	geofence       int
	markSeen       int
	statusWords    []statusObs
	protocol       string
	overspeed      []iot.Ping
	pending        *iot.DeviceCommand
	snapshot       iot.VehicleSnapshot
	sentCommands   []string
	refusedReasons []string
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

func (f *fakeStore) RecordStatusWord(_ context.Context, deviceID int64, frameType, statusWord, sampleFrame string, hadFix bool) error {
	f.mu.Lock()
	f.statusWords = append(f.statusWords, statusObs{
		deviceID: deviceID, frameType: frameType, word: statusWord,
		sample: sampleFrame, hadFix: hadFix,
	})
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) SetDeviceProtocol(_ context.Context, _ int64, protocol string) error {
	f.mu.Lock()
	f.protocol = protocol
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) counts() (insert, hot, mark int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inserted), len(f.hotState), f.markSeen
}

func (f *fakeStore) status() []statusObs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]statusObs(nil), f.statusWords...)
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
	// Validity 'A' but 0,0 coordinates. HQ clones emit this on cold start and
	// indoors, and because ParseHQFrame classifies by frame shape rather than
	// type code, a heartbeat carrying a position-shaped payload arrives the same
	// way. Either would land the vehicle on Null Island if inserted.
	frameZeroCoords    = "*HQ,9170503816,V1,123506,A,0000.0000,N,00000.0000,E,000.00,000,131216,FFFFFBFF#"
	frameZeroHeartbeat = "*HQ,9170503816,XT,120005,A,0000.0000,N,00000.0000,E,000.00,000,170826,FFFFFBFF#"
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

// A frame claiming a valid fix at 0,0 must be treated like a no-fix frame: the
// link stays warm but nothing is written. Before this guard the validity flag
// was the only check, so these were inserted and the vehicle jumped to the Gulf
// of Guinea on the live map.
func TestHandleZeroCoordinatesSkipInsert(t *testing.T) {
	for name, frame := range map[string]string{
		"position frame at 0,0":                 frameZeroCoords,
		"heartbeat shaped as a position at 0,0": frameZeroHeartbeat,
	} {
		t.Run(name, func(t *testing.T) {
			s := storeWith(&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true})
			runHandle(t, s, frame)

			insert, hot, mark := s.counts()
			if insert != 0 {
				t.Fatalf("0,0 frame must not write pings, got %d", insert)
			}
			if hot != 0 {
				t.Fatalf("0,0 frame must not move vehicle hot-state, got %d", hot)
			}
			if mark == 0 {
				t.Fatalf("expected the device to still be marked seen")
			}
		})
	}
}

// The status/alarm word must be captured for frames the gateway SKIPS, not just
// ones it ingests. Power-cut and tamper alarms arrive without a GPS lock, so if
// capture were tied to a successful ping insert, the frames carrying the most
// useful bits would be exactly the ones lost — and the per-model bit layout
// could never be derived from a pilot.
func TestHandleRecordsStatusWordOnSkippedFrames(t *testing.T) {
	cases := []struct {
		name      string
		frame     string
		wantType  string
		wantFix   bool
		wantPings int
	}{
		{"no-fix frame", frameNoFix, "V1", false, 0},
		{"zero-coordinate frame", frameZeroCoords, "V1", false, 0},
		{"heartbeat shaped as a position", frameZeroHeartbeat, "XT", false, 0},
		{"ingested frame still records", frameBound, "V1", true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := storeWith(&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true})
			runHandle(t, s, tc.frame)

			obs := s.status()
			if len(obs) != 1 {
				t.Fatalf("expected exactly 1 status observation, got %d", len(obs))
			}
			if obs[0].word != "FFFFFBFF" {
				t.Errorf("status word = %q, want FFFFFBFF", obs[0].word)
			}
			if obs[0].frameType != tc.wantType {
				t.Errorf("frame type = %q, want %q", obs[0].frameType, tc.wantType)
			}
			// hadFix distinguishes a state bit that accompanies a position from
			// an alarm-only frame — the key signal when correlating the bitfield.
			if obs[0].hadFix != tc.wantFix {
				t.Errorf("hadFix = %v, want %v", obs[0].hadFix, tc.wantFix)
			}
			if obs[0].sample == "" {
				t.Error("expected a sample frame kept for offline decoding")
			}
			if insert, _, _ := s.counts(); insert != tc.wantPings {
				t.Errorf("pings inserted = %d, want %d", insert, tc.wantPings)
			}
		})
	}
}

func TestHandleRecordsProtocolOnConnect(t *testing.T) {
	s := storeWith(&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true})
	runHandle(t, s, frameBound)

	s.mu.Lock()
	got := s.protocol
	s.mu.Unlock()
	if got != iot.ProtocolHQ {
		t.Fatalf("device protocol = %q, want %q", got, iot.ProtocolHQ)
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

func (f *fakeStore) ApplyOverspeed(_ context.Context, p iot.Ping, _ iot.OverspeedConfig) error {
	f.mu.Lock()
	f.overspeed = append(f.overspeed, p)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) NextPendingCommand(_ context.Context, _ int64) (*iot.DeviceCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending, nil
}

func (f *fakeStore) VehicleSnapshotFor(_ context.Context, _ string) (iot.VehicleSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot, nil
}

func (f *fakeStore) MarkCommandSent(_ context.Context, id int64, payload string, _ iot.VehicleSnapshot) error {
	f.mu.Lock()
	f.sentCommands = append(f.sentCommands, payload)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) MarkCommandRefused(_ context.Context, id int64, reason string, _ iot.VehicleSnapshot) error {
	f.mu.Lock()
	f.refusedReasons = append(f.refusedReasons, reason)
	f.mu.Unlock()
	return nil
}

// Enabling commands is not enough to send one: without a verified per-model
// encoder the gateway must refuse. This is the guard that stops a guessed byte
// sequence ever reaching a relay.
func TestDeliverCommand_refusesWithoutVerifiedEncoder(t *testing.T) {
	s := storeWith(&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true, Model: "ST-UNVERIFIED"})
	s.pending = &iot.DeviceCommand{
		ID: 7, DeviceID: 1, VehicleID: "VEH-001", Kind: iot.CommandImmobilize,
		Status: iot.CommandPending, RequestedAt: time.Now(),
	}
	zero := 0.0
	s.snapshot = iot.VehicleSnapshot{SpeedKmh: &zero, FixAge: time.Second, HasFix: true}

	srv, cli := net.Pipe()
	g := &hqGateway{store: s, sem: make(chan struct{}, 1),
		interlock: iot.DefaultInterlockConfig(), commandsEnabled: true}
	done := make(chan struct{})
	go func() { g.handle(srv); close(done) }()
	_, _ = io.WriteString(cli, frameBound)
	_ = cli.Close()
	<-done

	s.mu.Lock()
	sent, refused := len(s.sentCommands), append([]string(nil), s.refusedReasons...)
	s.mu.Unlock()

	if sent != 0 {
		t.Fatalf("nothing may be sent without a verified encoder, got %d writes", sent)
	}
	if len(refused) != 1 {
		t.Fatalf("expected exactly one recorded refusal, got %d", len(refused))
	}
}

// A moving vehicle must be refused even when everything else is in place.
func TestDeliverCommand_refusesMovingVehicle(t *testing.T) {
	iot.RegisterCommandEncoder("ST-MOVETEST", func(string) ([]byte, error) { return []byte("CUT"), nil })

	s := storeWith(&iot.Device{ID: 1, Serial: "9170503816", VehicleID: "VEH-001", IsActive: true, Model: "ST-MOVETEST"})
	s.pending = &iot.DeviceCommand{
		ID: 8, DeviceID: 1, VehicleID: "VEH-001", Kind: iot.CommandImmobilize,
		Status: iot.CommandPending, RequestedAt: time.Now(),
	}
	fast := 72.0
	s.snapshot = iot.VehicleSnapshot{SpeedKmh: &fast, FixAge: time.Second, HasFix: true}

	srv, cli := net.Pipe()
	g := &hqGateway{store: s, sem: make(chan struct{}, 1),
		interlock: iot.DefaultInterlockConfig(), commandsEnabled: true}
	done := make(chan struct{})
	go func() { g.handle(srv); close(done) }()
	_, _ = io.WriteString(cli, frameBound)
	_ = cli.Close()
	<-done

	s.mu.Lock()
	sent, refused := len(s.sentCommands), append([]string(nil), s.refusedReasons...)
	s.mu.Unlock()

	if sent != 0 {
		t.Fatal("must not immobilise a vehicle doing 72 km/h")
	}
	if len(refused) != 1 || !contains(refused[0], "moving") {
		t.Fatalf("expected a refusal mentioning movement, got %v", refused)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
