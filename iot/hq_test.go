package iot

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseHQPositionFrame(t *testing.T) {
	frame := "*HQ,9170503816,V1,123506,A,2232.6024,N,11355.7983,E,021.60,090,131216,FFFFFBFF,460,00,10342,4283#"
	msg, err := ParseHQFrame(frame)
	if err != nil {
		t.Fatalf("ParseHQFrame: %v", err)
	}
	if msg.DeviceID != "9170503816" {
		t.Errorf("DeviceID = %q, want 9170503816", msg.DeviceID)
	}
	if msg.Type != "V1" {
		t.Errorf("Type = %q, want V1", msg.Type)
	}
	if !msg.IsPosition || !msg.ValidFix {
		t.Fatalf("IsPosition=%v ValidFix=%v, want both true", msg.IsPosition, msg.ValidFix)
	}
	// 2232.6024 N → 22 + 32.6024/60
	wantLat := 22 + 32.6024/60
	if abs(msg.Lat-wantLat) > 1e-6 {
		t.Errorf("Lat = %v, want %v", msg.Lat, wantLat)
	}
	// 11355.7983 E → 113 + 55.7983/60
	wantLng := 113 + 55.7983/60
	if abs(msg.Lng-wantLng) > 1e-6 {
		t.Errorf("Lng = %v, want %v", msg.Lng, wantLng)
	}
	// 21.60 knots → 40.003 km/h
	if abs(msg.SpeedKmh-21.60*1.852) > 1e-6 {
		t.Errorf("SpeedKmh = %v, want %v", msg.SpeedKmh, 21.60*1.852)
	}
	if msg.Heading != 90 {
		t.Errorf("Heading = %v, want 90", msg.Heading)
	}
	want := time.Date(2016, 12, 13, 12, 35, 6, 0, time.UTC)
	if !msg.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", msg.Timestamp, want)
	}
	if msg.Status != "FFFFFBFF" {
		t.Errorf("Status = %q, want FFFFFBFF", msg.Status)
	}
}

func TestParseHQSouthWestHemisphere(t *testing.T) {
	// Nairobi-ish: south latitude, east longitude — and a Kampala-style west case.
	frame := "*HQ,8001,V1,080000,A,0117.4500,S,03649.2000,E,000.00,000,190626,FFFFFBFF#"
	msg, err := ParseHQFrame(frame)
	if err != nil {
		t.Fatalf("ParseHQFrame: %v", err)
	}
	if msg.Lat >= 0 {
		t.Errorf("Lat = %v, want negative (S)", msg.Lat)
	}
	if msg.Lng <= 0 {
		t.Errorf("Lng = %v, want positive (E)", msg.Lng)
	}
	wantLat := -(1 + 17.45/60)
	if abs(msg.Lat-wantLat) > 1e-6 {
		t.Errorf("Lat = %v, want %v", msg.Lat, wantLat)
	}
}

func TestParseHQInvalidFix(t *testing.T) {
	frame := "*HQ,8001,V1,080000,V,0000.0000,N,00000.0000,E,000.00,000,190626,FFFFFBFF#"
	msg, err := ParseHQFrame(frame)
	if err != nil {
		t.Fatalf("ParseHQFrame: %v", err)
	}
	if !msg.IsPosition {
		t.Errorf("IsPosition = false, want true (it is a position-shaped frame)")
	}
	if msg.ValidFix {
		t.Errorf("ValidFix = true, want false for 'V'")
	}
}

func TestParseHQHeartbeatIsNotPosition(t *testing.T) {
	for _, frame := range []string{
		"*HQ,9170503816,XT,123506#",
		"*HQ,9170503816,XT#",
	} {
		msg, err := ParseHQFrame(frame)
		if err != nil {
			t.Fatalf("ParseHQFrame(%q): %v", frame, err)
		}
		if msg.IsPosition {
			t.Errorf("ParseHQFrame(%q): IsPosition = true, want false", frame)
		}
		if msg.DeviceID != "9170503816" {
			t.Errorf("ParseHQFrame(%q): DeviceID = %q", frame, msg.DeviceID)
		}
	}
}

func TestParseHQRejectsBadHeader(t *testing.T) {
	if _, err := ParseHQFrame("$GPRMC,123519,A,4807.038,N#"); err == nil {
		t.Fatal("expected error for non-HQ header")
	}
	if _, err := ParseHQFrame(""); err == nil {
		t.Fatal("expected error for empty frame")
	}
}

func TestHQRawJSON(t *testing.T) {
	msg := HQMessage{Type: "V1", Status: "FFFFFBFF"}
	got := string(msg.RawJSON())
	if !strings.Contains(got, `"hqType":"V1"`) || !strings.Contains(got, `"hqStatus":"FFFFFBFF"`) {
		t.Errorf("RawJSON = %s, missing fields", got)
	}
	// Status omitted when absent.
	if got := string(HQMessage{Type: "XT"}.RawJSON()); strings.Contains(got, "hqStatus") {
		t.Errorf("RawJSON = %s, should omit hqStatus", got)
	}
}

func TestScanHQFramesStream(t *testing.T) {
	// Two frames back-to-back, plus inter-frame CR/LF noise and a trailing
	// partial frame with no terminator (should be dropped at EOF).
	stream := "*HQ,1,V1,080000,A,0100.0000,N,03600.0000,E,000.00,000,190626,0#\r\n" +
		"*HQ,1,XT#" +
		"*HQ,1,V1,080001,A,0100.0001,N,036"
	sc := NewHQScanner(bufio.NewReader(strings.NewReader(stream)))
	var frames []string
	for sc.Scan() {
		frames = append(frames, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d complete frames, want 2: %q", len(frames), frames)
	}
	m0, err := ParseHQFrame(frames[0])
	if err != nil || !m0.IsPosition {
		t.Errorf("frame 0 parse: msg=%+v err=%v", m0, err)
	}
	m1, err := ParseHQFrame(frames[1])
	if err != nil || m1.IsPosition || m1.Type != "XT" {
		t.Errorf("frame 1 parse: msg=%+v err=%v", m1, err)
	}
}

// Device health rides in the optional trailing fields and was being parsed
// past and discarded — which is why the platform could say where a tracker was
// but nothing about the tracker itself: no battery, no signal, no way to tell a
// flat unit from an unplugged one.
func TestParseHQFrame_trailingHealthFields(t *testing.T) {
	// A real ST-901 V1 frame, captured from a live unit.
	const frame = "*HQ,866104011702712,V1,160416,A,0020.8560,N,03234.9500,E,024.30,060,110926,FFFFFDFF,639,10,100,200,5#"
	msg, err := ParseHQFrame(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !msg.IsPosition {
		t.Fatal("expected a position frame")
	}
	for _, tc := range []struct {
		name string
		got  *int
		want int
	}{
		{"mcc", msg.MCC, 639},
		{"mnc", msg.MNC, 10},
		{"lac", msg.LAC, 100},
		{"cellId", msg.CellID, 200},
		{"batteryLevel", msg.BatteryLevel, 5},
	} {
		if tc.got == nil {
			t.Fatalf("%s not parsed", tc.name)
		}
		if *tc.got != tc.want {
			t.Fatalf("%s = %d, want %d", tc.name, *tc.got, tc.want)
		}
	}
}

// A device that does not send these must yield nil, never 0. The whole purpose
// of the fields is monitoring, and a fabricated zero reads as "battery flat" on
// a unit that simply never reported one.
func TestParseHQFrame_absentHealthFieldsStayNil(t *testing.T) {
	// Same frame truncated after the status word — the layout older clones send.
	const frame = "*HQ,866104011702712,V1,160416,A,0020.8560,N,03234.9500,E,024.30,060,110926,FFFFFDFF#"
	msg, err := ParseHQFrame(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !msg.IsPosition {
		t.Fatal("expected a position frame")
	}
	for name, got := range map[string]*int{
		"mcc": msg.MCC, "mnc": msg.MNC, "lac": msg.LAC,
		"cellId": msg.CellID, "batteryLevel": msg.BatteryLevel,
	} {
		if got != nil {
			t.Fatalf("%s should be nil when absent, got %d", name, *got)
		}
	}
	// The status word is still read — absence of the tail must not lose it.
	if msg.Status != "FFFFFDFF" {
		t.Fatalf("status = %q, want FFFFFDFF", msg.Status)
	}
}

// Blank and non-numeric trailing fields are gaps, not values.
func TestParseHQFrame_junkHealthFieldsStayNil(t *testing.T) {
	const frame = "*HQ,866104011702712,V1,160416,A,0020.8560,N,03234.9500,E,024.30,060,110926,FFFFFDFF,,x,100,200,#"
	msg, err := ParseHQFrame(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msg.MCC != nil {
		t.Fatalf("blank mcc should be nil, got %d", *msg.MCC)
	}
	if msg.MNC != nil {
		t.Fatalf("non-numeric mnc should be nil, got %d", *msg.MNC)
	}
	if msg.BatteryLevel != nil {
		t.Fatalf("blank battery should be nil, got %d", *msg.BatteryLevel)
	}
	// The readable ones in between are still kept.
	if msg.LAC == nil || *msg.LAC != 100 {
		t.Fatal("lac should still parse when a neighbour is junk")
	}
}

// The raw blob is what reaches telemetry_timeseries.raw, so the health fields
// have to survive into it — and absent ones must not appear at all.
func TestHQRawJSON_carriesHealthAndOmitsAbsent(t *testing.T) {
	full, err := ParseHQFrame("*HQ,8661,V1,160416,A,0020.8560,N,03234.9500,E,024.30,060,110926,FFFFFDFF,639,10,100,200,5#")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(full.RawJSON(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["batteryLevel"] != float64(5) {
		t.Fatalf("batteryLevel = %v, want 5", got["batteryLevel"])
	}
	if got["cellId"] != float64(200) {
		t.Fatalf("cellId = %v, want 200", got["cellId"])
	}
	if got["hqStatus"] != "FFFFFDFF" {
		t.Fatalf("hqStatus = %v", got["hqStatus"])
	}

	bare, err := ParseHQFrame("*HQ,8661,V1,160416,A,0020.8560,N,03234.9500,E,024.30,060,110926,FFFFFDFF#")
	if err != nil {
		t.Fatalf("parse bare: %v", err)
	}
	var lean map[string]any
	if err := json.Unmarshal(bare.RawJSON(), &lean); err != nil {
		t.Fatalf("unmarshal bare: %v", err)
	}
	for _, key := range []string{"batteryLevel", "mcc", "mnc", "lac", "cellId"} {
		if _, present := lean[key]; present {
			t.Fatalf("%s must be absent, not zero, when the device did not send it", key)
		}
	}
}
