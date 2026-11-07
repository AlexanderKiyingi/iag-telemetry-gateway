package iot

// GT06 / Concox binary protocol (SeeWorld and many Chinese trackers).
//
// Wire format (short frame):
//
//	[0x78 0x78][len 1][proto][content N][serial 2][crc 2][0x0D 0x0A]
//
// Extended frames use start 0x79 0x79 and a 2-byte length. CRC-ITU covers
// bytes from the length field through the information serial number.
//
// Protocol numbers we decode for live tracking:
//
//	0x01 login (IMEI as BCD) — server must ACK or the unit reconnects forever
//	0x12 GPS+LBS location
//	0x22 GT06N / SeeWorld extended location (same GPS header as 0x12)
//	0x13 heartbeat / status — server must ACK
//	0x16 alarm (GPS block + terminal status) — server must ACK
//
// SeeWorld SKUs overwhelmingly speak this family; JT808 units use a separate
// listener. Location body layout can still vary by firmware — DecodeGT06GPS
// accepts an optional model hint for future variants; the default path matches
// the Concox GT06 v1.8.1 GPS block.

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

const (
	gt06StartShort    = 0x7878
	gt06StartExtended = 0x7979
	gt06Stop0         = 0x0D
	gt06Stop1         = 0x0A

	GT06ProtoLogin    byte = 0x01
	GT06ProtoGPS      byte = 0x12
	GT06ProtoStatus   byte = 0x13
	GT06ProtoString   byte = 0x15
	GT06ProtoAlarm    byte = 0x16
	GT06ProtoGPSPhone byte = 0x1A
	GT06ProtoGPSExt   byte = 0x22 // GT06N / many SeeWorld 4G units
)

var (
	ErrGT06BadFrame = errors.New("gt06: bad frame")
	ErrGT06BadCRC   = errors.New("gt06: CRC mismatch")
)

// GT06Message is one decoded GT06 frame after CRC validation.
type GT06Message struct {
	Protocol   byte
	Serial     uint16
	IMEI       string // set on login
	IsPosition bool
	ValidFix   bool
	Timestamp  time.Time
	Lat        float64
	Lng        float64
	SpeedKmh   float64
	Heading    float64
	Satellites int
	// TerminalInfo is the status byte from heartbeat/alarm (ACC, charge, GPS, …).
	TerminalInfo *byte
	AlarmCode    *byte
	RawHex       string
	Content      []byte
}

// NeedsACK reports whether the server must reply or the device will tear down.
func (m GT06Message) NeedsACK() bool {
	switch m.Protocol {
	case GT06ProtoLogin, GT06ProtoStatus, GT06ProtoAlarm:
		return true
	default:
		return false
	}
}

// UsablePosition is true when the frame carries a GPS fix we can store.
func (m GT06Message) UsablePosition() bool {
	return m.IsPosition && m.ValidFix && !(m.Lat == 0 && m.Lng == 0) &&
		!math.IsNaN(m.Lat) && !math.IsNaN(m.Lng)
}

// RawJSON stashes protocol extras into telemetry_timeseries.raw.
func (m GT06Message) RawJSON() json.RawMessage {
	out := map[string]any{
		"gt06Protocol": m.Protocol,
		"gt06Serial":   m.Serial,
	}
	if m.TerminalInfo != nil {
		out["terminalInfo"] = *m.TerminalInfo
	}
	if m.AlarmCode != nil {
		out["alarmCode"] = *m.AlarmCode
	}
	if m.Satellites > 0 {
		out["satellites"] = m.Satellites
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// IgnitionFromTerminalInfo extracts ACC from the Concox terminal-information byte
// (bit 1). Returns nil when no terminal info was present.
func (m GT06Message) IgnitionFromTerminalInfo() *bool {
	if m.TerminalInfo == nil {
		return nil
	}
	v := (*m.TerminalInfo & 0x02) != 0
	return &v
}

// crc16ITU is the CRC-ITU used by Concox GT06 (CRC-16/X-25).
func crc16ITU(data []byte) uint16 {
	var crc uint16 = 0xFFFF
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}

// BuildGT06ACK builds a short-frame server response for login/status/alarm.
func BuildGT06ACK(protocol byte, serial uint16) []byte {
	body := []byte{0x05, protocol, byte(serial >> 8), byte(serial)}
	crc := crc16ITU(body)
	out := make([]byte, 0, 10)
	out = append(out, 0x78, 0x78)
	out = append(out, body...)
	out = append(out, byte(crc>>8), byte(crc))
	out = append(out, gt06Stop0, gt06Stop1)
	return out
}

// ReadGT06Frame reads one complete GT06 frame from r, syncing past noise.
func ReadGT06Frame(r *bufio.Reader) ([]byte, error) {
	for {
		b0, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b0 != 0x78 && b0 != 0x79 {
			continue
		}
		b1, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b0 == 0x78 && b1 == 0x78 {
			return readGT06Short(r)
		}
		if b0 == 0x79 && b1 == 0x79 {
			return readGT06Extended(r)
		}
		// Second byte might itself be a start — push back only b1's meaning by
		// unread when possible; otherwise continue scanning.
		if b1 == 0x78 || b1 == 0x79 {
			_ = r.UnreadByte()
		}
	}
}

func readGT06Short(r *bufio.Reader) ([]byte, error) {
	lenByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	// length includes proto+content+serial+crc (= 5+N). Remaining after length byte: that many.
	n := int(lenByte)
	if n < 5 || n > 512 {
		return nil, fmt.Errorf("%w: short length %d", ErrGT06BadFrame, n)
	}
	rest := make([]byte, n+2) // body + stop
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	if rest[n] != gt06Stop0 || rest[n+1] != gt06Stop1 {
		return nil, fmt.Errorf("%w: missing stop bits", ErrGT06BadFrame)
	}
	frame := make([]byte, 0, 2+1+n+2)
	frame = append(frame, 0x78, 0x78, lenByte)
	frame = append(frame, rest[:n]...)
	frame = append(frame, rest[n:]...)
	return frame, nil
}

func readGT06Extended(r *bufio.Reader) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n < 5 || n > 2048 {
		return nil, fmt.Errorf("%w: extended length %d", ErrGT06BadFrame, n)
	}
	rest := make([]byte, n+2)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	if rest[n] != gt06Stop0 || rest[n+1] != gt06Stop1 {
		return nil, fmt.Errorf("%w: missing stop bits", ErrGT06BadFrame)
	}
	frame := make([]byte, 0, 2+2+n+2)
	frame = append(frame, 0x79, 0x79)
	frame = append(frame, lenBuf[:]...)
	frame = append(frame, rest[:n]...)
	frame = append(frame, rest[n:]...)
	return frame, nil
}

// ParseGT06Frame validates CRC and decodes a complete frame (including start/stop).
// model is optional and reserved for layout variants (e.g. S5L); empty uses default.
func ParseGT06Frame(frame []byte, model string) (GT06Message, error) {
	var zero GT06Message
	if len(frame) < 10 {
		return zero, fmt.Errorf("%w: too short", ErrGT06BadFrame)
	}
	extended := frame[0] == 0x79 && frame[1] == 0x79
	short := frame[0] == 0x78 && frame[1] == 0x78
	if !short && !extended {
		return zero, fmt.Errorf("%w: bad start", ErrGT06BadFrame)
	}
	if frame[len(frame)-2] != gt06Stop0 || frame[len(frame)-1] != gt06Stop1 {
		return zero, fmt.Errorf("%w: bad stop", ErrGT06BadFrame)
	}

	var (
		payload []byte // length … serial (CRC input)
		body    []byte // proto + content + serial
	)
	if short {
		n := int(frame[2])
		if len(frame) != 2+1+n+2 {
			return zero, fmt.Errorf("%w: length mismatch", ErrGT06BadFrame)
		}
		// Declared length n = proto+content+serial+crc. CRC input = len through serial.
		crcGot := binary.BigEndian.Uint16(frame[3+n-2 : 3+n])
		payload = frame[2 : 3+n-2]
		if crc16ITU(payload) != crcGot {
			return zero, fmt.Errorf("%w: got 0x%04X", ErrGT06BadCRC, crcGot)
		}
		body = frame[3 : 3+n-2]
	} else {
		n := int(binary.BigEndian.Uint16(frame[2:4]))
		if len(frame) != 2+2+n+2 {
			return zero, fmt.Errorf("%w: extended length mismatch", ErrGT06BadFrame)
		}
		crcGot := binary.BigEndian.Uint16(frame[4+n-2 : 4+n])
		payload = frame[2 : 4+n-2]
		if crc16ITU(payload) != crcGot {
			return zero, fmt.Errorf("%w: got 0x%04X", ErrGT06BadCRC, crcGot)
		}
		body = frame[4 : 4+n-2]
	}

	if len(body) < 3 {
		return zero, fmt.Errorf("%w: empty body", ErrGT06BadFrame)
	}
	proto := body[0]
	serial := binary.BigEndian.Uint16(body[len(body)-2:])
	content := body[1 : len(body)-2]

	msg := GT06Message{
		Protocol: proto,
		Serial:   serial,
		Content:  append([]byte(nil), content...),
		RawHex:   strings.ToUpper(hex.EncodeToString(frame)),
	}

	switch proto {
	case GT06ProtoLogin:
		imei, err := decodeGT06IMEI(content)
		if err != nil {
			return zero, err
		}
		msg.IMEI = imei
	case GT06ProtoGPS, GT06ProtoGPSExt, GT06ProtoGPSPhone:
		if err := decodeGT06Location(&msg, content, model); err != nil {
			return zero, err
		}
	case GT06ProtoAlarm:
		if err := decodeGT06Alarm(&msg, content, model); err != nil {
			return zero, err
		}
	case GT06ProtoStatus:
		decodeGT06Status(&msg, content)
	}

	return msg, nil
}

func decodeGT06IMEI(content []byte) (string, error) {
	if len(content) < 8 {
		return "", fmt.Errorf("%w: login content too short", ErrGT06BadFrame)
	}
	var b strings.Builder
	b.Grow(16)
	for _, x := range content[:8] {
		b.WriteByte('0' + (x >> 4))
		b.WriteByte('0' + (x & 0x0F))
	}
	raw := b.String()
	// Prefer the canonical 15-digit IMEI: Concox packs IMEI into 8 BCD bytes
	// with a leading 0 nibble, so the last 15 digits of the 16-nibble string
	// are the IMEI operators register.
	if len(raw) >= 15 {
		return raw[len(raw)-15:], nil
	}
	s := strings.TrimLeft(raw, "0")
	if s == "" {
		return "", fmt.Errorf("%w: empty IMEI", ErrGT06BadFrame)
	}
	return s, nil
}

// GT06SerialCandidates lists serial forms to try against iot_devices.serial.
// Operators may have stored the IMEI with or without a leading zero.
func GT06SerialCandidates(imei string) []string {
	imei = strings.TrimSpace(imei)
	if imei == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(imei)
	add(strings.TrimLeft(imei, "0"))
	if len(imei) < 15 {
		add(strings.Repeat("0", 15-len(imei)) + imei)
	}
	if len(imei) == 15 {
		add("0" + imei)
	}
	return out
}

func decodeGT06Location(msg *GT06Message, content []byte, model string) error {
	gps, err := decodeGT06GPSBlock(content, model)
	if err != nil {
		return err
	}
	*msg = mergeGT06GPS(*msg, gps)
	msg.IsPosition = true
	return nil
}

func decodeGT06Alarm(msg *GT06Message, content []byte, model string) error {
	// Alarm: GPS block (same as 0x12) then LBS then terminal info / voltage / GSM / alarm.
	gps, err := decodeGT06GPSBlock(content, model)
	if err != nil {
		return err
	}
	*msg = mergeGT06GPS(*msg, gps)
	msg.IsPosition = gps.ValidFix
	// After GPS(6+1+4+4+1+2=18) + MCC(2)+MNC(1)+LAC(2)+Cell(3)=10 → offset 28 for terminal info on classic 0x16.
	if len(content) >= 29 {
		ti := content[28]
		msg.TerminalInfo = &ti
	}
	if len(content) >= 32 {
		ac := content[31] // alarm language high byte often carries alarm type in low nibble layouts; keep raw
		msg.AlarmCode = &ac
	}
	return nil
}

func decodeGT06Status(msg *GT06Message, content []byte) {
	if len(content) >= 1 {
		ti := content[0]
		msg.TerminalInfo = &ti
	}
	if len(content) >= 4 {
		ac := content[3] // alarm/language low byte on classic heartbeat
		msg.AlarmCode = &ac
	}
}

type gt06GPS struct {
	ValidFix   bool
	Timestamp  time.Time
	Lat, Lng   float64
	SpeedKmh   float64
	Heading    float64
	Satellites int
}

func mergeGT06GPS(msg GT06Message, g gt06GPS) GT06Message {
	msg.ValidFix = g.ValidFix
	msg.Timestamp = g.Timestamp
	msg.Lat = g.Lat
	msg.Lng = g.Lng
	msg.SpeedKmh = g.SpeedKmh
	msg.Heading = g.Heading
	msg.Satellites = g.Satellites
	return msg
}

// decodeGT06GPSBlock parses the Concox GPS information prefix present in 0x12/0x22/0x16.
func decodeGT06GPSBlock(content []byte, model string) (gt06GPS, error) {
	// model selects layout variants (e.g. S5L vs classic GT06N) when golden
	// frames prove the GPS block offsets diverge. Default Concox layout below
	// covers the SeeWorld catalogue until a fixture forces a branch.
	_ = strings.TrimSpace(model)
	var zero gt06GPS
	if len(content) < 18 {
		return zero, fmt.Errorf("%w: GPS content too short (%d)", ErrGT06BadFrame, len(content))
	}
	ts := time.Date(
		2000+int(content[0]), time.Month(content[1]), int(content[2]),
		int(content[3]), int(content[4]), int(content[5]), 0, time.UTC,
	)
	sats := int(content[6] & 0x0F)
	latRaw := binary.BigEndian.Uint32(content[7:11])
	lngRaw := binary.BigEndian.Uint32(content[11:15])
	speed := float64(content[15])
	courseStatus := binary.BigEndian.Uint16(content[16:18])

	lat := float64(latRaw) / 30000.0 / 60.0
	lng := float64(lngRaw) / 30000.0 / 60.0
	// BYTE_1 of course/status is the high byte.
	hi := byte(courseStatus >> 8)
	course := float64(courseStatus & 0x03FF)
	north := (hi & 0x04) != 0
	east := (hi & 0x08) == 0
	positioned := (hi & 0x10) != 0
	if !north {
		lat = -lat
	}
	if !east {
		lng = -lng
	}

	return gt06GPS{
		ValidFix:   positioned,
		Timestamp:  ts,
		Lat:        lat,
		Lng:        lng,
		SpeedKmh:   speed,
		Heading:    course,
		Satellites: sats,
	}, nil
}

// GT06MessageToPing maps a usable position into the shared Ping shape.
func GT06MessageToPing(msg GT06Message, device *Device) Ping {
	devID := device.ID
	heading := msg.Heading
	speed := msg.SpeedKmh
	p := Ping{
		VehicleID: device.VehicleID,
		DeviceID:  &devID,
		TS:        msg.Timestamp,
		Lat:       msg.Lat,
		Lng:       msg.Lng,
		Heading:   &heading,
		SpeedKmh:  &speed,
		Raw:       msg.RawJSON(),
	}
	if msg.Satellites > 0 {
		s := msg.Satellites
		p.Satellites = &s
	}
	if ign := msg.IgnitionFromTerminalInfo(); ign != nil {
		p.Ignition = ign
	}
	if msg.Timestamp.IsZero() {
		p.TS = time.Now().UTC()
	}
	return p
}
