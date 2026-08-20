package iot

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// The framing is checked byte-for-byte against the published Codec 12 layout,
// because this is what gets written to a truck's tracker. A field in the wrong
// place is either ignored by the device or means something else entirely.
func TestBuildCodec12Command_layout(t *testing.T) {
	frame, err := BuildCodec12Command("setdigout 1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if got := binary.BigEndian.Uint32(frame[0:4]); got != 0 {
		t.Errorf("preamble = %#08x, want 0", got)
	}
	dataLen := int(binary.BigEndian.Uint32(frame[4:8]))
	if want := 8 + dataLen + 4; len(frame) != want {
		t.Fatalf("frame is %d bytes, want %d (8 header + %d data + 4 crc)", len(frame), want, dataLen)
	}

	data := frame[8 : 8+dataLen]
	if data[0] != CodecID12 {
		t.Errorf("codec id = 0x%02X, want 0x0C", data[0])
	}
	if data[1] != 0x01 {
		t.Errorf("command quantity = %d, want 1", data[1])
	}
	if data[2] != codec12TypeCommand {
		t.Errorf("type = 0x%02X, want 0x05 (command)", data[2])
	}
	cmdLen := int(binary.BigEndian.Uint32(data[3:7]))
	if cmdLen != len("setdigout 1") {
		t.Errorf("command size = %d, want %d", cmdLen, len("setdigout 1"))
	}
	if got := string(data[7 : 7+cmdLen]); got != "setdigout 1" {
		t.Errorf("command = %q", got)
	}
	// The trailing quantity must repeat the leading one; devices reject
	// mismatches.
	if data[len(data)-1] != 0x01 {
		t.Errorf("trailing quantity = %d, want 1", data[len(data)-1])
	}

	// CRC is over the data field only, not the header.
	gotCRC := binary.BigEndian.Uint32(frame[8+dataLen:]) & 0xFFFF
	if want := uint32(crc16IBM(data)); gotCRC != want {
		t.Errorf("crc = 0x%04X, want 0x%04X", gotCRC, want)
	}
}

func TestBuildCodec12Command_rejectsBadInput(t *testing.T) {
	if _, err := BuildCodec12Command(""); !errors.Is(err, ErrCodec12Empty) {
		t.Errorf("empty command: err = %v, want ErrCodec12Empty", err)
	}
	if _, err := BuildCodec12Command(strings.Repeat("x", maxCodec12Command+1)); !errors.Is(err, ErrCodec12TooLong) {
		t.Errorf("oversized command: err = %v, want ErrCodec12TooLong", err)
	}
}

// A frame we build must be parseable by the same rules, with the type byte
// flipped to "response" — the round trip catches an inconsistency between the
// two halves.
func TestParseCodec12Response_roundTrip(t *testing.T) {
	frame, err := BuildCodec12Command("Digital output 1 set to 1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	dataLen := int(binary.BigEndian.Uint32(frame[4:8]))
	// Flip command → response and re-CRC.
	frame[8+2] = codec12TypeResponse
	data := frame[8 : 8+dataLen]
	binary.BigEndian.PutUint32(frame[8+dataLen:], uint32(crc16IBM(data)))

	got, err := ParseCodec12Response(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != "Digital output 1 set to 1" {
		t.Fatalf("response = %q", got)
	}
}

func TestParseCodec12Response_rejectsMalformed(t *testing.T) {
	good, _ := BuildCodec12Command("ok")
	dataLen := int(binary.BigEndian.Uint32(good[4:8]))
	good[8+2] = codec12TypeResponse
	binary.BigEndian.PutUint32(good[8+dataLen:], uint32(crc16IBM(good[8:8+dataLen])))

	corrupt := func(mutate func([]byte)) []byte {
		cp := append([]byte(nil), good...)
		mutate(cp)
		return cp
	}

	cases := map[string][]byte{
		"truncated":     good[:10],
		"bad preamble":  corrupt(func(b []byte) { b[0] = 0xFF }),
		"corrupted crc": corrupt(func(b []byte) { b[len(b)-1] ^= 0xFF }),
		"wrong codec":   corrupt(func(b []byte) { b[8] = 0x08 }),
		// A silently-truncated payload must not be read as a short response.
		"oversized declared size": corrupt(func(b []byte) {
			binary.BigEndian.PutUint32(b[4:8], 0xFFFF)
		}),
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCodec12Response(frame); err == nil {
				t.Fatalf("expected a parse error for %s (frame %s)", name, hex.EncodeToString(frame))
			}
		})
	}
}
