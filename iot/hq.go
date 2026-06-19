package iot

// SinoTrack / HQ-protocol parser.
//
// SinoTrack trackers (ST-901, ST-906, ST-915, and the many GT06-era clones
// that share their firmware) do not speak Teltonika Codec 8. They emit ASCII
// frames in the "HQ" protocol, delimited by '*' … '#':
//
//	*HQ,9170503816,V1,123506,A,2232.6024,N,11355.7983,E,000.00,000,131216,FFFFFBFF,460,00,10342,4283#
//
// Comma-separated fields:
//	[0]  HQ          vendor header (after the leading '*')
//	[1]  9170503816  device id — matches iot_devices.serial (cf. Teltonika IMEI)
//	[2]  V1          message type (V1 periodic, V4 alarm/response, XT heartbeat …)
//	[3]  123506      time  HHMMSS (UTC)
//	[4]  A           fix validity: 'A' valid, 'V' no fix
//	[5]  2232.6024   latitude  DDMM.MMMM
//	[6]  N           N / S
//	[7]  11355.7983  longitude DDDMM.MMMM
//	[8]  E           E / W
//	[9]  000.00      speed over ground, knots
//	[10] 000         heading, degrees
//	[11] 131216      date  DDMMYY
//	[12] FFFFFBFF    status/alarm bitfield, 8 hex chars (optional)
//	[13…]            mcc, mnc, lac, cellid … (optional, firmware-dependent)
//
// Unlike Teltonika there is no length-prefixed IMEI handshake and no CRC: the
// device id rides in every frame and the gateway authenticates on the first
// one it sees. Frames are fire-and-forget; the server is not required to ACK a
// position report, so the connection is kept alive by TCP/firmware keepalive.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

var (
	ErrHQEmptyFrame   = errors.New("hq: empty frame")
	ErrHQBadHeader    = errors.New("hq: frame does not start with *HQ")
	ErrHQBadHemi      = errors.New("hq: hemisphere is not one of N/S/E/W")
	ErrHQFrameTooLong = errors.New("hq: frame exceeds maximum length — likely garbage")
)

// maxHQFrame caps a single frame. Real HQ frames are well under 200 bytes; a
// stream that never sends '#' must not let the scanner buffer grow unbounded.
const maxHQFrame = 1024

// HQMessage is one decoded HQ-protocol frame.
type HQMessage struct {
	DeviceID   string    // id the tracker sends; matched against iot_devices.serial
	Type       string    // V1, V4, XT, …
	IsPosition bool      // true when the frame carries a GPS-fix layout
	ValidFix   bool      // 'A' → true, 'V' (no lock) → false
	Timestamp  time.Time // UTC; zero when the frame carries no date/time
	Lat        float64
	Lng        float64
	SpeedKmh   float64
	Heading    float64
	Status     string // raw status/alarm hex, "" when absent
	Raw        string // the frame body between '*' and '#'
}

// ParseHQFrame decodes a single frame. The input may include the framing
// ('*' … '#'), surrounding whitespace, or a trailing CR/LF — all are trimmed.
//
// Whether a frame is a position report is decided structurally (validity flag +
// N/S + E/W in the expected slots), not by trusting the type string, so V1 /
// V4 / V0 location layouts are all handled and heartbeats are not mistaken for
// fixes.
func ParseHQFrame(frame string) (HQMessage, error) {
	body := strings.TrimSpace(frame)
	body = strings.Trim(body, "*#\r\n ")
	if body == "" {
		return HQMessage{}, ErrHQEmptyFrame
	}
	fields := strings.Split(body, ",")
	if len(fields) < 3 || !strings.EqualFold(fields[0], "HQ") {
		return HQMessage{}, ErrHQBadHeader
	}

	msg := HQMessage{
		DeviceID: fields[1],
		Type:     strings.ToUpper(fields[2]),
		Raw:      body,
	}

	// A position report needs at least through the date field (index 11), with
	// the validity flag and both hemisphere markers in their expected slots.
	// Anything else (heartbeat, login, command echo) is returned as a non-
	// position control frame the caller can ignore without erroring.
	if len(fields) < 12 || !isHQHemisphere(fields[6]) || !isHQHemisphere(fields[8]) {
		return msg, nil
	}
	validity := strings.ToUpper(strings.TrimSpace(fields[4]))
	if validity != "A" && validity != "V" {
		return msg, nil
	}

	msg.IsPosition = true
	msg.ValidFix = validity == "A"

	lat, err := hqCoord(fields[5], fields[6])
	if err != nil {
		return HQMessage{}, fmt.Errorf("hq: latitude: %w", err)
	}
	lng, err := hqCoord(fields[7], fields[8])
	if err != nil {
		return HQMessage{}, fmt.Errorf("hq: longitude: %w", err)
	}
	msg.Lat, msg.Lng = lat, lng

	if knots, err := strconv.ParseFloat(strings.TrimSpace(fields[9]), 64); err == nil {
		msg.SpeedKmh = knots * 1.852 // 1 knot = 1.852 km/h
	}
	if hdg, err := strconv.ParseFloat(strings.TrimSpace(fields[10]), 64); err == nil {
		msg.Heading = hdg
	}

	ts, err := hqTimestamp(fields[11], fields[3])
	if err != nil {
		return HQMessage{}, fmt.Errorf("hq: timestamp: %w", err)
	}
	msg.Timestamp = ts

	if len(fields) > 12 {
		msg.Status = strings.TrimSpace(fields[12])
	}
	return msg, nil
}

// RawJSON renders the protocol-specific extras (type, status bitfield) into the
// JSON blob stored in telemetry_timeseries.raw, mirroring how the Teltonika
// gateway stows its IO map. The status word's bit layout varies across the HQ
// clones, so it is preserved verbatim rather than decoded into ignition/alarm
// fields here; downstream consumers can interpret it per device model.
func (m HQMessage) RawJSON() json.RawMessage {
	out := map[string]string{"hqType": m.Type}
	if m.Status != "" {
		out["hqStatus"] = m.Status
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func isHQHemisphere(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "N", "S", "E", "W":
		return true
	}
	return false
}

// hqCoord converts an HQ DDMM.MMMM / DDDMM.MMMM value plus hemisphere into
// signed decimal degrees. The integer part is whole degrees ×100 plus minutes;
// e.g. 2232.6024 N → 22° + 32.6024′/60 = 22.543373°.
func hqCoord(raw, hemi string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, err
	}
	deg := math.Trunc(v / 100)
	minutes := v - deg*100
	dec := deg + minutes/60
	switch strings.ToUpper(strings.TrimSpace(hemi)) {
	case "N", "E":
		return dec, nil
	case "S", "W":
		return -dec, nil
	default:
		return 0, ErrHQBadHemi
	}
}

// hqTimestamp combines the DDMMYY date and HHMMSS time fields into a UTC time.
// Two-digit years are mapped into the 2000s — these devices have no 20th-century use.
func hqTimestamp(ddmmyy, hhmmss string) (time.Time, error) {
	if len(ddmmyy) != 6 || len(hhmmss) != 6 {
		return time.Time{}, fmt.Errorf("expected DDMMYY+HHMMSS, got %q + %q", ddmmyy, hhmmss)
	}
	day, e1 := strconv.Atoi(ddmmyy[0:2])
	month, e2 := strconv.Atoi(ddmmyy[2:4])
	year, e3 := strconv.Atoi(ddmmyy[4:6])
	hh, e4 := strconv.Atoi(hhmmss[0:2])
	mm, e5 := strconv.Atoi(hhmmss[2:4])
	ss, e6 := strconv.Atoi(hhmmss[4:6])
	if err := firstErr(e1, e2, e3, e4, e5, e6); err != nil {
		return time.Time{}, err
	}
	if day < 1 || day > 31 || month < 1 || month > 12 || hh > 23 || mm > 59 || ss > 59 {
		return time.Time{}, fmt.Errorf("field out of range in %q %q", ddmmyy, hhmmss)
	}
	return time.Date(2000+year, time.Month(month), day, hh, mm, ss, 0, time.UTC), nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// ScanHQFrames is a bufio.SplitFunc that yields one HQ frame per token,
// delimited by the trailing '#'. Bytes before a frame's leading '*' (stray
// CR/LF or keepalive noise between frames) are carried into the token and
// stripped by ParseHQFrame. Frames longer than maxHQFrame are rejected so a
// peer that never sends '#' cannot grow the scanner buffer without bound.
func ScanHQFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := indexByte(data, '#'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if len(data) > maxHQFrame {
		return 0, nil, ErrHQFrameTooLong
	}
	if atEOF {
		// Trailing bytes with no terminator — drop them.
		return len(data), nil, nil
	}
	return 0, nil, nil // request more data
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// NewHQScanner wraps r in a bufio.Scanner configured for HQ frames.
func NewHQScanner(r *bufio.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256), maxHQFrame+1)
	sc.Split(ScanHQFrames)
	return sc
}
