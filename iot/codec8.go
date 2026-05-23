package iot

// Teltonika Codec 8 / Codec 8E parser.
//
// Spec reference: https://wiki.teltonika-gps.com/view/Codec
//
// The protocol on TCP works in three phases:
//   1. IMEI handshake. Device sends [2-byte length][ASCII IMEI];
//      server replies a single byte: 0x01 (accept) or 0x00 (reject).
//   2. AVL data packets. Device sends:
//        [4 bytes preamble = 0x00000000]
//        [4 bytes data-field length]
//        [1 byte codec ID = 0x08 or 0x8E]
//        [1 byte record count]
//        records[]
//        [1 byte record count] (must match)
//        [4 bytes CRC-16/IBM, low 2 bytes carry the checksum]
//      Server ACKs with a 4-byte big-endian count of records accepted.
//   3. Loop on (2) until disconnect.
//
// All multi-byte numeric fields are big-endian. Coordinates are signed
// 32-bit integers in 1e-7 degrees; speed is uint16 km/h (0xFFFF means
// "unknown"); altitude is int16 metres.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	CodecID8  byte = 0x08
	CodecID8E byte = 0x8E
)

// AVLRecord is one position fix from the device. IOs holds the full IO
// element map keyed by ID (string-encoded so it survives JSON round-trip).
type AVLRecord struct {
	Timestamp  time.Time
	Priority   uint8
	Longitude  float64
	Latitude   float64
	Altitude   int16
	Angle      uint16
	Satellites uint8
	Speed      uint16          // km/h; 0xFFFF means unknown
	EventIOID  uint16          // 1 byte for codec 8, 2 bytes for codec 8E
	IOs        map[uint16]int64 // value coerced to int64; original size preserved in IOSizes
}

var (
	ErrHandshakeTooLong = errors.New("codec8: IMEI longer than 64 bytes — likely garbage")
	ErrBadPreamble      = errors.New("codec8: AVL packet preamble is not 0x00000000")
	ErrUnknownCodec     = errors.New("codec8: unknown codec ID (expected 0x08 or 0x8E)")
	ErrCountMismatch    = errors.New("codec8: leading and trailing record counts differ")
	ErrBadCRC           = errors.New("codec8: CRC mismatch")
)

// ReadHandshake reads the IMEI length-prefixed ASCII string sent on
// connect. Returns the IMEI; the caller decides whether to accept.
func ReadHandshake(r io.Reader) (string, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return "", err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n == 0 || n > 64 {
		return "", ErrHandshakeTooLong
	}
	imei := make([]byte, n)
	if _, err := io.ReadFull(r, imei); err != nil {
		return "", err
	}
	return string(imei), nil
}

// WriteHandshakeResponse writes the single-byte accept/reject reply.
func WriteHandshakeResponse(w io.Writer, accepted bool) error {
	b := byte(0x00)
	if accepted {
		b = 0x01
	}
	_, err := w.Write([]byte{b})
	return err
}

// WriteACK writes a 4-byte big-endian count, the protocol's record-accepted
// acknowledgement after a successful AVL packet.
func WriteACK(w io.Writer, count int) error {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(count))
	_, err := w.Write(buf[:])
	return err
}

// ReadAVLPacket parses one AVL packet from r and returns the records plus
// the codec ID that produced them. Verifies preamble, count match, and CRC.
func ReadAVLPacket(r *bufio.Reader) (codec byte, records []AVLRecord, err error) {
	// preamble (4) + data field length (4)
	var head [8]byte
	if _, err = io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	if binary.BigEndian.Uint32(head[0:4]) != 0 {
		return 0, nil, ErrBadPreamble
	}
	dataLen := binary.BigEndian.Uint32(head[4:8])
	if dataLen == 0 || dataLen > 1<<20 {
		return 0, nil, fmt.Errorf("codec8: implausible data field length %d", dataLen)
	}

	// Read the entire data field + CRC into memory so we can re-CRC and
	// rewind without juggling Reader state.
	body := make([]byte, dataLen)
	if _, err = io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	var crcBuf [4]byte
	if _, err = io.ReadFull(r, crcBuf[:]); err != nil {
		return 0, nil, err
	}
	gotCRC := binary.BigEndian.Uint32(crcBuf[:]) & 0xFFFF
	wantCRC := uint32(crc16IBM(body))
	if gotCRC != wantCRC {
		return 0, nil, fmt.Errorf("%w: got 0x%04X want 0x%04X", ErrBadCRC, gotCRC, wantCRC)
	}

	codec = body[0]
	if codec != CodecID8 && codec != CodecID8E {
		return 0, nil, ErrUnknownCodec
	}
	count1 := body[1]
	cur := 2

	records = make([]AVLRecord, 0, count1)
	for i := byte(0); i < count1; i++ {
		rec, n, perr := parseRecord(body[cur:], codec)
		if perr != nil {
			return 0, nil, fmt.Errorf("record %d: %w", i, perr)
		}
		records = append(records, rec)
		cur += n
	}

	if cur >= len(body) {
		return 0, nil, fmt.Errorf("codec8: truncated packet")
	}
	count2 := body[cur]
	if count2 != count1 {
		return 0, nil, ErrCountMismatch
	}
	return codec, records, nil
}

func parseRecord(b []byte, codec byte) (AVLRecord, int, error) {
	var rec AVLRecord
	if len(b) < 24 {
		return rec, 0, fmt.Errorf("short record header")
	}
	tsMs := int64(binary.BigEndian.Uint64(b[0:8]))
	rec.Timestamp = time.UnixMilli(tsMs).UTC()
	rec.Priority = b[8]

	// GPS element: lng, lat, altitude, angle, sats, speed (15 bytes)
	rec.Longitude = float64(int32(binary.BigEndian.Uint32(b[9:13]))) / 1e7
	rec.Latitude = float64(int32(binary.BigEndian.Uint32(b[13:17]))) / 1e7
	rec.Altitude = int16(binary.BigEndian.Uint16(b[17:19]))
	rec.Angle = binary.BigEndian.Uint16(b[19:21])
	rec.Satellites = b[21]
	rec.Speed = binary.BigEndian.Uint16(b[22:24])

	rec.IOs = make(map[uint16]int64)

	cur := 24
	if codec == CodecID8 {
		// 1-byte event ID + 1-byte total count, then 4 size-class blocks
		// each of: 1-byte N + N × (1-byte ID + size bytes value)
		if len(b) < cur+2 {
			return rec, 0, fmt.Errorf("short IO header")
		}
		rec.EventIOID = uint16(b[cur])
		// b[cur+1] is total IO count — informational, not load-bearing for parsing
		cur += 2

		for _, size := range []int{1, 2, 4, 8} {
			if cur >= len(b) {
				return rec, 0, fmt.Errorf("short IO size-block %d", size)
			}
			n := int(b[cur])
			cur++
			needed := n * (1 + size)
			if cur+needed > len(b) {
				return rec, 0, fmt.Errorf("short IO body size=%d n=%d", size, n)
			}
			for j := 0; j < n; j++ {
				id := uint16(b[cur])
				cur++
				rec.IOs[id] = readIntBE(b[cur : cur+size])
				cur += size
			}
		}
	} else { // Codec 8E
		// 2-byte event ID + 2-byte total count, then 4 fixed-size blocks
		// (sizes 1/2/4/8) with 2-byte counts and 2-byte IO IDs, then a
		// variable-size block.
		if len(b) < cur+4 {
			return rec, 0, fmt.Errorf("short 8E IO header")
		}
		rec.EventIOID = binary.BigEndian.Uint16(b[cur : cur+2])
		// b[cur+2:cur+4] is total IO count — informational
		cur += 4

		for _, size := range []int{1, 2, 4, 8} {
			if cur+2 > len(b) {
				return rec, 0, fmt.Errorf("short 8E size-block %d", size)
			}
			n := int(binary.BigEndian.Uint16(b[cur : cur+2]))
			cur += 2
			needed := n * (2 + size)
			if cur+needed > len(b) {
				return rec, 0, fmt.Errorf("short 8E IO body size=%d n=%d", size, n)
			}
			for j := 0; j < n; j++ {
				id := binary.BigEndian.Uint16(b[cur : cur+2])
				cur += 2
				rec.IOs[id] = readIntBE(b[cur : cur+size])
				cur += size
			}
		}
		// Variable-length IO block
		if cur+2 > len(b) {
			return rec, 0, fmt.Errorf("short 8E variable header")
		}
		nx := int(binary.BigEndian.Uint16(b[cur : cur+2]))
		cur += 2
		for j := 0; j < nx; j++ {
			if cur+4 > len(b) {
				return rec, 0, fmt.Errorf("short 8E variable element header")
			}
			// id (2) + length (2) + payload (length)
			length := int(binary.BigEndian.Uint16(b[cur+2 : cur+4]))
			cur += 4 + length
			if cur > len(b) {
				return rec, 0, fmt.Errorf("short 8E variable payload")
			}
			// Variable IOs are device-specific (CAN frames, etc); skip the
			// payload but advance the cursor. Could be persisted into raw
			// JSON later if needed.
		}
	}
	return rec, cur, nil
}

func readIntBE(b []byte) int64 {
	switch len(b) {
	case 1:
		return int64(int8(b[0]))
	case 2:
		return int64(int16(binary.BigEndian.Uint16(b)))
	case 4:
		return int64(int32(binary.BigEndian.Uint32(b)))
	case 8:
		return int64(binary.BigEndian.Uint64(b))
	}
	return 0
}

// crc16IBM computes the CRC-16/IBM (poly 0xA001, init 0x0000, no reflection,
// no xorout) used by Teltonika's protocol. Implemented bit-by-bit; this is
// fine for AVL packets which are at most a few KB.
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
