package iot

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// buildCodec8Packet constructs a syntactically valid single-record Codec 8
// packet with the given GPS values and computes the CRC. Used by the
// round-trip tests below — easier to maintain than hand-rolled hex strings.
func buildCodec8Packet(t *testing.T, ts time.Time, lng, lat float64, altitude int16, angle uint16, sats uint8, speed uint16) []byte {
	t.Helper()
	body := make([]byte, 0, 64)

	body = append(body, CodecID8)
	body = append(body, 0x01) // count1

	// Record
	tsMs := ts.UnixMilli()
	body = appendU64(body, uint64(tsMs))
	body = append(body, 0x01) // priority

	body = appendU32(body, uint32(int32(lng*1e7)))
	body = appendU32(body, uint32(int32(lat*1e7)))
	body = appendU16(body, uint16(altitude))
	body = appendU16(body, angle)
	body = append(body, sats)
	body = appendU16(body, speed)

	// IO element: no IOs at all.
	body = append(body, 0x00) // event id
	body = append(body, 0x00) // total count
	body = append(body, 0x00) // N1
	body = append(body, 0x00) // N2
	body = append(body, 0x00) // N4
	body = append(body, 0x00) // N8

	body = append(body, 0x01) // count2

	// Wrap in preamble + length + CRC
	out := make([]byte, 0, len(body)+12)
	out = appendU32(out, 0)
	out = appendU32(out, uint32(len(body)))
	out = append(out, body...)
	crc := uint32(crc16IBM(body))
	out = appendU32(out, crc)
	return out
}

func appendU16(b []byte, v uint16) []byte {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], v)
	return append(b, buf[:]...)
}
func appendU32(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}
func appendU64(b []byte, v uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	return append(b, buf[:]...)
}

func TestParseCodec8RoundTrip(t *testing.T) {
	want := struct {
		ts    time.Time
		lng   float64
		lat   float64
		alt   int16
		angle uint16
		sats  uint8
		speed uint16
	}{
		ts:    time.Unix(1_560_160_861, 0).UTC(),
		lng:   25.2619848,
		lat:   54.7999132,
		alt:   205,
		angle: 87,
		sats:  9,
		speed: 64,
	}
	raw := buildCodec8Packet(t, want.ts, want.lng, want.lat, want.alt, want.angle, want.sats, want.speed)

	r := bufio.NewReader(bytes.NewReader(raw))
	codec, recs, err := ReadAVLPacket(r)
	if err != nil {
		t.Fatalf("ReadAVLPacket: %v", err)
	}
	if codec != CodecID8 {
		t.Errorf("codec = %#x, want 0x08", codec)
	}
	if len(recs) != 1 {
		t.Fatalf("len(recs) = %d, want 1", len(recs))
	}
	got := recs[0]
	if !got.Timestamp.Equal(want.ts) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, want.ts)
	}
	if abs(got.Longitude-want.lng) > 1e-7 {
		t.Errorf("Longitude = %v, want %v", got.Longitude, want.lng)
	}
	if abs(got.Latitude-want.lat) > 1e-7 {
		t.Errorf("Latitude = %v, want %v", got.Latitude, want.lat)
	}
	if got.Altitude != want.alt {
		t.Errorf("Altitude = %d, want %d", got.Altitude, want.alt)
	}
	if got.Angle != want.angle {
		t.Errorf("Angle = %d, want %d", got.Angle, want.angle)
	}
	if got.Satellites != want.sats {
		t.Errorf("Satellites = %d, want %d", got.Satellites, want.sats)
	}
	if got.Speed != want.speed {
		t.Errorf("Speed = %d, want %d", got.Speed, want.speed)
	}
}

func TestHandshake(t *testing.T) {
	imei := "356307042441013"
	raw := append([]byte{0x00, 0x0F}, []byte(imei)...)
	got, err := ReadHandshake(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadHandshake: %v", err)
	}
	if got != imei {
		t.Errorf("got %q want %q", got, imei)
	}

	var w bytes.Buffer
	if err := WriteHandshakeResponse(&w, true); err != nil {
		t.Fatal(err)
	}
	if w.Len() != 1 || w.Bytes()[0] != 0x01 {
		t.Errorf("accept reply = % x, want 01", w.Bytes())
	}

	w.Reset()
	if err := WriteHandshakeResponse(&w, false); err != nil {
		t.Fatal(err)
	}
	if w.Bytes()[0] != 0x00 {
		t.Errorf("reject reply = % x, want 00", w.Bytes())
	}
}

func TestACK(t *testing.T) {
	var w bytes.Buffer
	if err := WriteACK(&w, 7); err != nil {
		t.Fatal(err)
	}
	if got := w.Bytes(); len(got) != 4 || got[3] != 0x07 || got[0] != 0 {
		t.Errorf("ACK bytes = % x, want 00000007", got)
	}
}

func TestBadPreamble(t *testing.T) {
	raw := bytes.Repeat([]byte{0xFF}, 32)
	_, _, err := ReadAVLPacket(bufio.NewReader(bytes.NewReader(raw)))
	if err == nil {
		t.Fatal("expected error for non-zero preamble")
	}
}

func TestBadCRC(t *testing.T) {
	raw := buildCodec8Packet(t, time.Now().UTC(), 1, 2, 3, 4, 5, 6)
	raw[len(raw)-1] ^= 0xFF // corrupt the last byte of the CRC
	_, _, err := ReadAVLPacket(bufio.NewReader(bytes.NewReader(raw)))
	if err == nil {
		t.Fatal("expected CRC error")
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestCRC16IBMKnownVectors(t *testing.T) {
	cases := []struct {
		in   []byte
		want uint16
	}{
		{[]byte{}, 0x0000},
		// Single byte 0x01 → 0xC0C1 (verified against an external CRC-16/IBM calculator).
		{[]byte{0x01}, 0xC0C1},
		{[]byte("123456789"), 0xBB3D},
	}
	for _, tc := range cases {
		if got := crc16IBM(tc.in); got != tc.want {
			t.Errorf("crc16IBM(%q) = 0x%04X, want 0x%04X", tc.in, got, tc.want)
		}
	}
}
