package iot

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestJT808EscapeRoundTrip(t *testing.T) {
	raw := []byte{0x01, 0x7E, 0x02, 0x7D, 0x03}
	framed := EscapeJT808(raw)
	got, err := UnescapeJT808(framed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("got %v want %v", got, raw)
	}
}

func TestJT808RegisterAndLocationRoundTrip(t *testing.T) {
	phone := "13800138000"
	reg := buildJT808Packet(JT808MsgRegister, phone, 1, []byte("reg-body-padding!!")) // body ignored for parse fields
	msg, err := ParseJT808Frame(reg)
	if err != nil {
		t.Fatal(err)
	}
	if msg.MsgID != JT808MsgRegister {
		t.Fatalf("msgId = 0x%04X", msg.MsgID)
	}
	if msg.Phone != "13800138000" {
		t.Fatalf("phone = %q", msg.Phone)
	}
	if !msg.NeedsReply() {
		t.Fatal("register needs reply")
	}

	// Build a valid 0x0200 location body.
	body := make([]byte, 28)
	binary.BigEndian.PutUint32(body[0:4], 0)    // alarm
	binary.BigEndian.PutUint32(body[4:8], 0x03) // ACC + positioned
	// 1.234567° → 1234567
	binary.BigEndian.PutUint32(body[8:12], 1234567)
	binary.BigEndian.PutUint32(body[12:16], 103456789)
	binary.BigEndian.PutUint16(body[16:18], 500)  // alt
	binary.BigEndian.PutUint16(body[18:20], 120)  // 12.0 km/h
	binary.BigEndian.PutUint16(body[20:22], 90)   // course
	copy(body[22:28], []byte{0x24, 0x03, 0x15, 0x10, 0x30, 0x00}) // 2024-03-15 10:30:00 BCD

	loc := buildJT808Packet(JT808MsgLocation, phone, 2, body)
	msg2, err := ParseJT808Frame(loc)
	if err != nil {
		t.Fatal(err)
	}
	if !msg2.UsablePosition() {
		t.Fatalf("expected usable position, valid=%v lat=%v lng=%v", msg2.ValidFix, msg2.Lat, msg2.Lng)
	}
	if abs64(msg2.Lat-1.234567) > 1e-6 {
		t.Fatalf("lat = %v", msg2.Lat)
	}
	if abs64(msg2.Lng-103.456789) > 1e-6 {
		t.Fatalf("lng = %v", msg2.Lng)
	}
	if abs64(msg2.SpeedKmh-12.0) > 1e-9 {
		t.Fatalf("speed = %v", msg2.SpeedKmh)
	}
	if msg2.Ignition == nil || !*msg2.Ignition {
		t.Fatal("expected ignition true")
	}
	wantTS := time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)
	if !msg2.Timestamp.Equal(wantTS) {
		t.Fatalf("ts = %v want %v", msg2.Timestamp, wantTS)
	}
}

func TestBuildJT808Reply_Register(t *testing.T) {
	msg := JT808Message{MsgID: JT808MsgRegister, Phone: "13800138000", Serial: 7}
	reply := BuildJT808Reply(msg, 1)
	parsed, err := ParseJT808Frame(reply)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.MsgID != JT808MsgRegisterResponse {
		t.Fatalf("reply msgId = 0x%04X", parsed.MsgID)
	}
}

func TestReadJT808Frame_Stream(t *testing.T) {
	phone := "8613800138000"
	a := buildJT808Packet(JT808MsgHeartbeat, phone, 1, nil)
	b := buildJT808Packet(JT808MsgHeartbeat, phone, 2, nil)
	stream := append(append([]byte{0x00, 0x11}, a...), b...)
	r := bufio.NewReader(bytes.NewReader(stream))
	f1, err := ReadJT808Frame(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f1, a) {
		t.Fatal("frame1 mismatch")
	}
	f2, err := ReadJT808Frame(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f2, b) {
		t.Fatal("frame2 mismatch")
	}
}

func TestParseJT808Frame_BadChecksum(t *testing.T) {
	phone := "13800138000"
	framed := buildJT808Packet(JT808MsgHeartbeat, phone, 1, nil)
	raw, err := UnescapeJT808(framed)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xFF
	bad := EscapeJT808(raw)
	_, err = ParseJT808Frame(bad)
	if err == nil {
		t.Fatal("expected checksum error")
	}
}
