package iot

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.ReplaceAll(s, " ", "")
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}

func TestCRC16ITU_ProtocolExamples(t *testing.T) {
	// Login CRC body: length through serial.
	loginBody := mustHex(t, "0D 01 01 23 45 67 89 01 23 45 00 01")
	if got := crc16ITU(loginBody); got != 0x8CDD {
		t.Fatalf("login crc = 0x%04X, want 0x8CDD", got)
	}
	ackBody := mustHex(t, "05 01 00 01")
	if got := crc16ITU(ackBody); got != 0xD9DC {
		t.Fatalf("ack crc = 0x%04X, want 0xD9DC", got)
	}
}

func TestParseGT06Frame_Login(t *testing.T) {
	frame := mustHex(t, "78 78 0D 01 01 23 45 67 89 01 23 45 00 01 8C DD 0D 0A")
	msg, err := ParseGT06Frame(frame, "")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Protocol != GT06ProtoLogin {
		t.Fatalf("protocol = 0x%02X", msg.Protocol)
	}
	if msg.IMEI != "123456789012345" {
		t.Fatalf("IMEI = %q, want 123456789012345", msg.IMEI)
	}
	if msg.Serial != 1 {
		t.Fatalf("serial = %d", msg.Serial)
	}
	if !msg.NeedsACK() {
		t.Fatal("login must need ACK")
	}
	cands := GT06SerialCandidates(msg.IMEI)
	if len(cands) < 2 {
		t.Fatalf("candidates = %v", cands)
	}
}

func TestBuildGT06ACK_LoginExample(t *testing.T) {
	got := BuildGT06ACK(GT06ProtoLogin, 1)
	want := mustHex(t, "78 78 05 01 00 01 D9 DC 0D 0A")
	if !bytes.Equal(got, want) {
		t.Fatalf("ack =\n%s\nwant\n%s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

func TestParseGT06Frame_Location(t *testing.T) {
	// Spec example location content (5.2.2) with CRC-ITU recomputed — the
	// published PDF example lists 0x8081 which does not match CRC-ITU over the
	// length…serial bytes (actual 0x7377).
	frame := mustHex(t, "78 78 1F 12 0B 08 1D 11 2E 10 CC 02 7A C7 EB 0C 46 58 49 00 14 8F 01 CC 00 28 7D 00 1F B8 00 03 73 77 0D 0A")
	msg, err := ParseGT06Frame(frame, "")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Protocol != GT06ProtoGPS {
		t.Fatalf("protocol = 0x%02X", msg.Protocol)
	}
	if !msg.IsPosition || !msg.ValidFix {
		t.Fatalf("expected usable fix flags, got position=%v valid=%v", msg.IsPosition, msg.ValidFix)
	}
	if msg.Satellites != 12 { // 0xCC → low nibble is 0xC = 12? Wait: high nibble length, low sats.
		// 0xCC: high=C=12 (gps info length), low=C=12 satellites
		t.Fatalf("satellites = %d, want 12", msg.Satellites)
	}
	// Lat raw 0x027AC7EB / 30000 / 60
	wantLat := float64(0x027AC7EB) / 30000.0 / 60.0
	// Course status 0x148F: bit2 of hi=1 → North; bit3=0 → East; bit4=1 positioned
	if diff := abs64(msg.Lat - wantLat); diff > 1e-6 {
		t.Fatalf("lat = %v, want %v", msg.Lat, wantLat)
	}
	wantLng := float64(0x0C465849) / 30000.0 / 60.0
	if diff := abs64(msg.Lng - wantLng); diff > 1e-6 {
		t.Fatalf("lng = %v, want %v", msg.Lng, wantLng)
	}
	if msg.SpeedKmh != 0 {
		t.Fatalf("speed = %v", msg.SpeedKmh)
	}
	if int(msg.Heading) != 0x08F { // low 10 bits of 0x148F = 0x08F = 143
		// 0x148F & 0x3FF = 0x08F = 143
		if int(msg.Heading) != 143 {
			t.Fatalf("heading = %v, want 143", msg.Heading)
		}
	}
	if !msg.UsablePosition() {
		t.Fatal("expected usable position")
	}
}

func TestParseGT06Frame_BadCRC(t *testing.T) {
	frame := mustHex(t, "78 78 0D 01 01 23 45 67 89 01 23 45 00 01 8C DE 0D 0A")
	_, err := ParseGT06Frame(frame, "")
	if err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("want CRC error, got %v", err)
	}
}

func TestReadGT06Frame_RoundTrip(t *testing.T) {
	login := mustHex(t, "78 78 0D 01 01 23 45 67 89 01 23 45 00 01 8C DD 0D 0A")
	loc := mustHex(t, "78 78 1F 12 0B 08 1D 11 2E 10 CC 02 7A C7 EB 0C 46 58 49 00 14 8F 01 CC 00 28 7D 00 1F B8 00 03 80 81 0D 0A")
	stream := append(append([]byte{0x00, 0xFF}, login...), loc...)
	r := bufio.NewReader(bytes.NewReader(stream))
	f1, err := ReadGT06Frame(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f1, login) {
		t.Fatalf("frame1 mismatch")
	}
	f2, err := ReadGT06Frame(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f2, loc) {
		t.Fatalf("frame2 mismatch")
	}
}

func TestParseGT06Frame_StatusNeedsACK(t *testing.T) {
	// Build a minimal status frame: len=0x0A proto=0x13 content=5 bytes serial + crc
	content := []byte{0x40, 0x04, 0x04, 0x00, 0x01} // terminal, voltage, gsm, alarm/lang
	serial := []byte{0x00, 0x05}
	bodyNoCRC := append([]byte{0x0A, GT06ProtoStatus}, content...)
	bodyNoCRC = append(bodyNoCRC, serial...)
	crc := crc16ITU(bodyNoCRC)
	frame := []byte{0x78, 0x78}
	frame = append(frame, bodyNoCRC...)
	frame = append(frame, byte(crc>>8), byte(crc), 0x0D, 0x0A)
	msg, err := ParseGT06Frame(frame, "")
	if err != nil {
		t.Fatal(err)
	}
	if !msg.NeedsACK() {
		t.Fatal("status must need ACK")
	}
	if msg.TerminalInfo == nil || *msg.TerminalInfo != 0x40 {
		t.Fatalf("terminalInfo = %v", msg.TerminalInfo)
	}
}

func abs64(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
