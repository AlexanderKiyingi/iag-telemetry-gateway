package iot

// JT/T 808 (JT808) binary protocol — used by some SeeWorld GPS units (P5, W20L)
// and many Chinese commercial trackers.
//
// Wire framing:
//
//	0x7E <escaped header+body+checksum> 0x7E
//
// Escape rules inside the payload: 0x7D 0x02 → 0x7E, 0x7D 0x01 → 0x7D.
// After unescape the packet is:
//
//	[msgId 2][bodyProps 2][phone BCD 6][serial 2][body N][checksum 1]
//
// Checksum is the XOR of every byte from msgId through the end of body.
//
// We implement the subset required for live tracking:
//
//	0x0100 terminal register → reply 0x8100
//	0x0102 terminal auth     → reply 0x8001 success
//	0x0002 heartbeat         → reply 0x8001 success
//	0x0200 location report   → reply 0x8001 success + Ping
//
// Terminal phone (BCD) is matched to iot_devices.serial (digits only).

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
	JT808MsgTerminalGeneralResponse uint16 = 0x0001
	JT808MsgHeartbeat               uint16 = 0x0002
	JT808MsgRegister                uint16 = 0x0100
	JT808MsgAuth                    uint16 = 0x0102
	JT808MsgLocation                uint16 = 0x0200

	JT808MsgPlatformGeneralResponse uint16 = 0x8001
	JT808MsgRegisterResponse        uint16 = 0x8100
)

var (
	ErrJT808BadFrame = errors.New("jt808: bad frame")
	ErrJT808BadXOR   = errors.New("jt808: checksum mismatch")
)

// JT808Message is one decoded JT808 packet after unescape + XOR check.
type JT808Message struct {
	MsgID      uint16
	Serial     uint16
	Phone      string // terminal phone / device id digits
	IsPosition bool
	ValidFix   bool
	Timestamp  time.Time
	Lat        float64
	Lng        float64
	Altitude   float64
	SpeedKmh   float64
	Heading    float64
	Ignition   *bool
	RawHex     string
	Body       []byte
}

// NeedsReply is true when the platform must answer or the session stalls.
func (m JT808Message) NeedsReply() bool {
	switch m.MsgID {
	case JT808MsgRegister, JT808MsgAuth, JT808MsgHeartbeat, JT808MsgLocation:
		return true
	default:
		return false
	}
}

// UsablePosition reports a GPS fix worth persisting.
func (m JT808Message) UsablePosition() bool {
	return m.IsPosition && m.ValidFix && !(m.Lat == 0 && m.Lng == 0) &&
		!math.IsNaN(m.Lat) && !math.IsNaN(m.Lng)
}

// RawJSON stashes protocol extras into telemetry_timeseries.raw.
func (m JT808Message) RawJSON() json.RawMessage {
	out := map[string]any{
		"jt808MsgId": fmt.Sprintf("0x%04X", m.MsgID),
		"jt808Serial": m.Serial,
		"jt808Phone":  m.Phone,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// ReadJT808Frame reads one 0x7E … 0x7E frame (still escaped, including delimiters).
func ReadJT808Frame(r *bufio.Reader) ([]byte, error) {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != 0x7E {
			continue
		}
		var buf []byte
		buf = append(buf, 0x7E)
		for {
			c, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			buf = append(buf, c)
			if c == 0x7E {
				if len(buf) < 5 {
					// Empty or delimiter-only — keep scanning from this 0x7E.
					buf = buf[:1]
					continue
				}
				return buf, nil
			}
			if len(buf) > 4096 {
				return nil, fmt.Errorf("%w: frame too large", ErrJT808BadFrame)
			}
		}
	}
}

// UnescapeJT808 removes JT808 byte stuffing from the interior of a framed packet.
func UnescapeJT808(framed []byte) ([]byte, error) {
	if len(framed) < 3 || framed[0] != 0x7E || framed[len(framed)-1] != 0x7E {
		return nil, fmt.Errorf("%w: missing delimiters", ErrJT808BadFrame)
	}
	inner := framed[1 : len(framed)-1]
	out := make([]byte, 0, len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] == 0x7D {
			if i+1 >= len(inner) {
				return nil, fmt.Errorf("%w: truncated escape", ErrJT808BadFrame)
			}
			switch inner[i+1] {
			case 0x01:
				out = append(out, 0x7D)
			case 0x02:
				out = append(out, 0x7E)
			default:
				return nil, fmt.Errorf("%w: bad escape 0x7D 0x%02X", ErrJT808BadFrame, inner[i+1])
			}
			i++
			continue
		}
		out = append(out, inner[i])
	}
	return out, nil
}

// EscapeJT808 applies JT808 stuffing to a raw (unescaped) packet body+header+xor.
func EscapeJT808(raw []byte) []byte {
	out := make([]byte, 0, len(raw)+8)
	out = append(out, 0x7E)
	for _, b := range raw {
		switch b {
		case 0x7E:
			out = append(out, 0x7D, 0x02)
		case 0x7D:
			out = append(out, 0x7D, 0x01)
		default:
			out = append(out, b)
		}
	}
	out = append(out, 0x7E)
	return out
}

func jt808XOR(data []byte) byte {
	var x byte
	for _, b := range data {
		x ^= b
	}
	return x
}

// ParseJT808Frame unescapes, checks XOR, and decodes a framed packet.
func ParseJT808Frame(framed []byte) (JT808Message, error) {
	var zero JT808Message
	raw, err := UnescapeJT808(framed)
	if err != nil {
		return zero, err
	}
	if len(raw) < 13 { // header 12 + checksum
		return zero, fmt.Errorf("%w: too short (%d)", ErrJT808BadFrame, len(raw))
	}
	payload, checksum := raw[:len(raw)-1], raw[len(raw)-1]
	if jt808XOR(payload) != checksum {
		return zero, fmt.Errorf("%w: got 0x%02X want 0x%02X", ErrJT808BadXOR, checksum, jt808XOR(payload))
	}

	msgID := binary.BigEndian.Uint16(payload[0:2])
	bodyProps := binary.BigEndian.Uint16(payload[2:4])
	phone := decodeJT808Phone(payload[4:10])
	serial := binary.BigEndian.Uint16(payload[10:12])

	bodyLen := int(bodyProps & 0x03FF)
	headerLen := 12
	// Sub-package bit (bit13): if set, 4 extra bytes after header.
	if bodyProps&0x2000 != 0 {
		headerLen = 16
	}
	if len(payload) < headerLen+bodyLen {
		return zero, fmt.Errorf("%w: body truncated", ErrJT808BadFrame)
	}
	body := payload[headerLen : headerLen+bodyLen]

	msg := JT808Message{
		MsgID:  msgID,
		Serial: serial,
		Phone:  phone,
		Body:   append([]byte(nil), body...),
		RawHex: strings.ToUpper(hex.EncodeToString(framed)),
	}

	switch msgID {
	case JT808MsgLocation:
		if err := decodeJT808Location(&msg, body); err != nil {
			return zero, err
		}
	}

	return msg, nil
}

func decodeJT808Phone(bcd []byte) string {
	var b strings.Builder
	b.Grow(12)
	for _, x := range bcd {
		hi, lo := x>>4, x&0x0F
		if hi <= 9 {
			b.WriteByte('0' + hi)
		}
		if lo <= 9 {
			b.WriteByte('0' + lo)
		}
	}
	raw := b.String()
	trimmed := strings.TrimLeft(raw, "0")
	if trimmed == "" {
		return raw
	}
	return trimmed
}

// JT808SerialCandidates lists terminal-phone forms to try against iot_devices.serial.
func JT808SerialCandidates(phone string) []string {
	phone = strings.TrimSpace(phone)
	if phone == "" {
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
	add(phone)
	add(strings.TrimLeft(phone, "0"))
	if len(phone) < 12 {
		add(strings.Repeat("0", 12-len(phone)) + phone)
	}
	if len(phone) < 15 {
		add(strings.Repeat("0", 15-len(phone)) + phone)
	}
	return out
}

func encodeJT808Phone(phone string) [6]byte {
	digits := make([]byte, 0, 12)
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits = append(digits, byte(r-'0'))
		}
	}
	for len(digits) < 12 {
		digits = append([]byte{0}, digits...)
	}
	if len(digits) > 12 {
		digits = digits[len(digits)-12:]
	}
	var out [6]byte
	for i := 0; i < 6; i++ {
		out[i] = digits[i*2]<<4 | digits[i*2+1]
	}
	return out
}

func decodeJT808Location(msg *JT808Message, body []byte) error {
	if len(body) < 28 {
		return fmt.Errorf("%w: location body too short", ErrJT808BadFrame)
	}
	status := binary.BigEndian.Uint32(body[4:8])
	latRaw := binary.BigEndian.Uint32(body[8:12])
	lngRaw := binary.BigEndian.Uint32(body[12:16])
	alt := float64(binary.BigEndian.Uint16(body[16:18]))
	speed := float64(binary.BigEndian.Uint16(body[18:20])) / 10.0
	course := float64(binary.BigEndian.Uint16(body[20:22]))
	ts, err := decodeJT808BCDTime(body[22:28])
	if err != nil {
		return err
	}

	lat := float64(latRaw) / 1e6
	lng := float64(lngRaw) / 1e6
	if status&0x04 != 0 { // south latitude
		lat = -lat
	}
	if status&0x08 != 0 { // west longitude
		lng = -lng
	}
	acc := status&0x01 != 0
	// Bit1: 0=idle? JT808 bit1 is often ACC; bit0 is ACC on some docs.
	// JT/T 808-2011: bit0 ACC, bit1 positioning (0 not positioned / 1 positioned) —
	// Clarification varies by revision. Common Traccar mapping:
	//   bit0 ACC, bit1 positioned.
	positioned := status&0x02 != 0

	msg.IsPosition = true
	msg.ValidFix = positioned
	msg.Timestamp = ts
	msg.Lat = lat
	msg.Lng = lng
	msg.Altitude = alt
	msg.SpeedKmh = speed
	msg.Heading = course
	msg.Ignition = &acc
	return nil
}

func decodeJT808BCDTime(b []byte) (time.Time, error) {
	if len(b) != 6 {
		return time.Time{}, fmt.Errorf("%w: bad BCD time", ErrJT808BadFrame)
	}
	decode := func(x byte) int { return int(x>>4)*10 + int(x&0x0F) }
	year := 2000 + decode(b[0])
	month := decode(b[1])
	day := decode(b[2])
	hour := decode(b[3])
	min := decode(b[4])
	sec := decode(b[5])
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC), nil
}

// BuildJT808PlatformGeneralResponse builds 0x8001 success for a terminal message.
func BuildJT808PlatformGeneralResponse(phone string, replySerial uint16, ackMsgID uint16, ackSerial uint16, result byte) []byte {
	body := make([]byte, 5)
	binary.BigEndian.PutUint16(body[0:2], ackSerial)
	binary.BigEndian.PutUint16(body[2:4], ackMsgID)
	body[4] = result // 0 = success
	return buildJT808Packet(JT808MsgPlatformGeneralResponse, phone, replySerial, body)
}

// BuildJT808RegisterResponse builds 0x8100 with result=0 and a short auth code.
func BuildJT808RegisterResponse(phone string, replySerial uint16, ackSerial uint16, authCode string) []byte {
	if authCode == "" {
		authCode = "IAG"
	}
	body := make([]byte, 3+len(authCode))
	binary.BigEndian.PutUint16(body[0:2], ackSerial)
	body[2] = 0 // success
	copy(body[3:], authCode)
	return buildJT808Packet(JT808MsgRegisterResponse, phone, replySerial, body)
}

func buildJT808Packet(msgID uint16, phone string, serial uint16, body []byte) []byte {
	phoneBCD := encodeJT808Phone(phone)
	raw := make([]byte, 0, 12+len(body)+1)
	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[0:2], msgID)
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(body)&0x03FF))
	copy(hdr[4:10], phoneBCD[:])
	binary.BigEndian.PutUint16(hdr[10:12], serial)
	raw = append(raw, hdr[:]...)
	raw = append(raw, body...)
	raw = append(raw, jt808XOR(raw))
	return EscapeJT808(raw)
}

// BuildJT808Reply selects the correct platform reply for a terminal message.
func BuildJT808Reply(msg JT808Message, platformSerial uint16) []byte {
	switch msg.MsgID {
	case JT808MsgRegister:
		return BuildJT808RegisterResponse(msg.Phone, platformSerial, msg.Serial, "IAG")
	case JT808MsgAuth, JT808MsgHeartbeat, JT808MsgLocation:
		return BuildJT808PlatformGeneralResponse(msg.Phone, platformSerial, msg.MsgID, msg.Serial, 0)
	default:
		return BuildJT808PlatformGeneralResponse(msg.Phone, platformSerial, msg.MsgID, msg.Serial, 0)
	}
}

// JT808MessageToPing maps a usable location into the shared Ping shape.
func JT808MessageToPing(msg JT808Message, device *Device) Ping {
	devID := device.ID
	heading := msg.Heading
	speed := msg.SpeedKmh
	alt := msg.Altitude
	p := Ping{
		VehicleID: device.VehicleID,
		DeviceID:  &devID,
		TS:        msg.Timestamp,
		Lat:       msg.Lat,
		Lng:       msg.Lng,
		Altitude:  &alt,
		Heading:   &heading,
		SpeedKmh:  &speed,
		Ignition:  msg.Ignition,
		Raw:       msg.RawJSON(),
	}
	if msg.Timestamp.IsZero() {
		p.TS = time.Now().UTC()
	}
	return p
}

// WriteJT808 writes a framed reply to w.
func WriteJT808(w io.Writer, framed []byte) error {
	_, err := w.Write(framed)
	return err
}
