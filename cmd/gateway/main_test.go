package main

import (
	"context"
	"encoding/binary"
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
	devices  map[string]*iot.Device // serial(IMEI) -> row
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

func storeWith(devs ...*iot.Device) *fakeStore {
	m := make(map[string]*iot.Device, len(devs))
	for _, d := range devs {
		m[d.Serial] = d
	}
	return &fakeStore{devices: m}
}

// ── binary builders (Codec 8 wire format; mirrors iot/codec8_test.go) ─────────

func handshakeFrame(imei string) []byte {
	out := make([]byte, 2, 2+len(imei))
	binary.BigEndian.PutUint16(out, uint16(len(imei)))
	return append(out, []byte(imei)...)
}

// codec8Packet builds a valid single-record Codec 8 AVL packet for the given
// position. lat/lng of 0 reproduce Teltonika's no-GPS-lock sentinel.
func codec8Packet(ts time.Time, lng, lat float64) []byte {
	body := make([]byte, 0, 64)
	body = append(body, iot.CodecID8)
	body = append(body, 0x01) // record count (leading)

	body = appendU64(body, uint64(ts.UnixMilli()))
	body = append(body, 0x01) // priority
	body = appendU32(body, uint32(int32(lng*1e7)))
	body = appendU32(body, uint32(int32(lat*1e7)))
	body = appendU16(body, 0)    // altitude
	body = appendU16(body, 0)    // angle
	body = append(body, 0x09)    // satellites
	body = appendU16(body, 0x000A) // speed (10 km/h)

	// IO element block: no IOs.
	body = append(body, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	body = append(body, 0x01) // record count (trailing)

	out := make([]byte, 0, len(body)+12)
	out = appendU32(out, 0) // preamble
	out = appendU32(out, uint32(len(body)))
	out = append(out, body...)
	out = appendU32(out, uint32(crc16IBM(body)))
	return out
}

func appendU16(b []byte, v uint16) []byte { return binary.BigEndian.AppendUint16(b, v) }
func appendU32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }
func appendU64(b []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(b, v) }

// crc16IBM mirrors the unexported parser CRC (poly 0xA001) so the test can build
// packets the gateway will accept.
func crc16IBM(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// runHandle drives one connection: write the frames, drain whatever the gateway
// writes back (handshake reply + ACKs — the synchronous pipe would otherwise
// block its writes), then wait for the loop to exit on peer close.
func runHandle(t *testing.T, store tcpStore, frames ...[]byte) {
	t.Helper()
	srv, cli := net.Pipe()
	g := &tcpGateway{store: store, sem: make(chan struct{}, 1)}

	done := make(chan struct{})
	go func() {
		g.handle(srv)
		close(done)
	}()
	go func() { _, _ = io.Copy(io.Discard, cli) }() // drain gateway → test

	for _, f := range frames {
		if _, err := cli.Write(f); err != nil {
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

const testIMEI = "356307042441013"

func TestHandleBoundDeviceValidFixIngests(t *testing.T) {
	s := storeWith(&iot.Device{ID: 1, Serial: testIMEI, VehicleID: "VEH-001", IsActive: true})
	runHandle(t, s, handshakeFrame(testIMEI), codec8Packet(time.Unix(1700000000, 0), 32.5825, 0.3476))

	insert, hot, mark := s.counts()
	if insert != 1 {
		t.Fatalf("expected 1 ping inserted, got %d", insert)
	}
	if hot != 1 {
		t.Fatalf("expected hot-state sync once, got %d", hot)
	}
	if s.geofence == 0 || mark == 0 {
		t.Fatalf("expected geofence eval + mark-seen (geofence=%d mark=%d)", s.geofence, mark)
	}
	if s.inserted[0].VehicleID != "VEH-001" {
		t.Errorf("ping vehicle = %q, want VEH-001", s.inserted[0].VehicleID)
	}
}

func TestHandleUnboundDeviceSkipsInsert(t *testing.T) {
	s := storeWith(&iot.Device{ID: 2, Serial: testIMEI, VehicleID: "", IsActive: true})
	runHandle(t, s, handshakeFrame(testIMEI), codec8Packet(time.Unix(1700000000, 0), 32.5825, 0.3476))

	insert, hot, _ := s.counts()
	if insert != 0 {
		t.Fatalf("unbound device must not write pings, got %d", insert)
	}
	if hot != 0 {
		t.Fatalf("unbound device must not sync hot-state, got %d", hot)
	}
}

func TestHandleZeroFixDropped(t *testing.T) {
	s := storeWith(&iot.Device{ID: 1, Serial: testIMEI, VehicleID: "VEH-001", IsActive: true})
	runHandle(t, s, handshakeFrame(testIMEI), codec8Packet(time.Unix(1700000000, 0), 0, 0))

	insert, hot, mark := s.counts()
	if insert != 0 {
		t.Fatalf("(0,0) no-fix record must be dropped, got %d inserted", insert)
	}
	if hot != 0 {
		t.Fatalf("no hot-state for an all-no-fix packet, got %d", hot)
	}
	if mark == 0 {
		t.Fatalf("device should still be marked seen on a no-fix packet")
	}
}

func TestHandleUnknownDeviceRejected(t *testing.T) {
	// Only the handshake is sent: the gateway rejects an unknown IMEI and closes
	// before any AVL packet, so it never reads (or could ingest) one.
	s := storeWith(&iot.Device{ID: 1, Serial: "999999999999999", VehicleID: "VEH-001", IsActive: true})
	runHandle(t, s, handshakeFrame(testIMEI))

	insert, _, mark := s.counts()
	if insert != 0 || mark != 0 {
		t.Fatalf("unknown device must be rejected at handshake: insert=%d mark=%d", insert, mark)
	}
}
