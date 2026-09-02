package iot

import (
	"bufio"
	"bytes"
	"testing"
	"time"
)

// buildCodec8EPacket constructs a single-record Codec 8E packet carrying one
// 1-byte fixed IO and a set of variable-length IO elements.
//
// 8E is the codec that has a variable block at all — Codec 8 has none — so the
// variable-element handling can only be exercised through it.
func buildCodec8EPacket(t *testing.T, fixedID uint16, fixedVal byte, varIOs map[uint16][]byte) []byte {
	t.Helper()
	body := make([]byte, 0, 128)
	body = append(body, CodecID8E)
	body = append(body, 0x01) // count1

	body = appendU64(body, uint64(time.Now().UnixMilli()))
	body = append(body, 0x01)                     // priority
	body = appendU32(body, uint32(int32(32.5e7))) // lng
	body = appendU32(body, uint32(int32(0.34e7))) // lat
	body = appendU16(body, 1200)                  // altitude
	body = appendU16(body, 90)                    // angle
	body = append(body, 9)                        // satellites
	body = appendU16(body, 42)                    // speed

	// 8E IO header: 2-byte event id + 2-byte total count.
	body = appendU16(body, 0)
	body = appendU16(body, uint16(1+len(varIOs)))

	// N1 block: one 1-byte element.
	body = appendU16(body, 1)
	body = appendU16(body, fixedID)
	body = append(body, fixedVal)
	// N2, N4, N8 blocks: empty.
	body = appendU16(body, 0)
	body = appendU16(body, 0)
	body = appendU16(body, 0)

	// Variable block: count, then id + length + payload each.
	body = appendU16(body, uint16(len(varIOs)))
	for id, payload := range varIOs {
		body = appendU16(body, id)
		body = appendU16(body, uint16(len(payload)))
		body = append(body, payload...)
	}

	body = append(body, 0x01) // count2

	out := make([]byte, 0, len(body)+12)
	out = appendU32(out, 0)
	out = appendU32(out, uint32(len(body)))
	out = append(out, body...)
	out = appendU32(out, uint32(crc16IBM(body)))
	return out
}

// Variable-length IO elements used to be parsed for their length and then
// thrown away — the cursor advanced past the payload and nothing kept it. On a
// CAN-adapter unit that is where some of the vehicle data lives, and because
// telemetry_timeseries.raw is the only record of what a device actually sent,
// discarding them meant a decoder written later could never be backfilled: the
// fleet would have to be re-driven to collect the data again.
func TestCodec8EKeepsVariableIOElements(t *testing.T) {
	want := map[uint16][]byte{
		0x0100: {0xDE, 0xAD, 0xBE, 0xEF},
		0x0200: {0x01, 0x02},
	}
	pkt := buildCodec8EPacket(t, 239, 1, want)

	codec, recs, err := ReadAVLPacket(bufio.NewReader(bytes.NewReader(pkt)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if codec != CodecID8E {
		t.Fatalf("codec = %#x, want 8E", codec)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]

	// The fixed element must still land in the numeric map.
	if got, ok := rec.IOs[239]; !ok || got != 1 {
		t.Errorf("fixed IO 239 = %v (present %v), want 1", got, ok)
	}

	if len(rec.VarIOs) != len(want) {
		t.Fatalf("got %d variable IOs, want %d", len(rec.VarIOs), len(want))
	}
	for id, payload := range want {
		got, ok := rec.VarIOs[id]
		if !ok {
			t.Errorf("variable IO %#x missing", id)
			continue
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("variable IO %#x = % x, want % x", id, got, payload)
		}
	}
}

// The cursor must still land exactly at the end of the record. Keeping the
// payload changed how the parser advances, and getting that wrong would
// misalign every following record in a multi-record packet rather than failing
// loudly.
func TestCodec8EVariableBlockAdvancesCursorCorrectly(t *testing.T) {
	pkt := buildCodec8EPacket(t, 239, 1, map[uint16][]byte{0x0100: {0xAA, 0xBB, 0xCC}})
	if _, _, err := ReadAVLPacket(bufio.NewReader(bytes.NewReader(pkt))); err != nil {
		t.Fatalf("a packet whose trailing record count must line up failed to parse: %v", err)
	}
}

// A zero-length variable element is legal on the wire and must not create an
// empty entry that a consumer would then have to special-case.
func TestCodec8EZeroLengthVariableElementIsSkipped(t *testing.T) {
	pkt := buildCodec8EPacket(t, 239, 1, map[uint16][]byte{0x0300: {}})
	_, recs, err := ReadAVLPacket(bufio.NewReader(bytes.NewReader(pkt)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs[0].VarIOs) != 0 {
		t.Errorf("zero-length element produced an entry: %v", recs[0].VarIOs)
	}
}

// Codec 8 has no variable block; VarIOs must stay nil rather than an empty map,
// so "this codec cannot carry them" and "it carried none" stay distinguishable.
func TestCodec8HasNoVariableIOs(t *testing.T) {
	pkt := buildCodec8Packet(t, time.Now(), 32.5, 0.34, 1200, 90, 9, 42)
	_, recs, err := ReadAVLPacket(bufio.NewReader(bytes.NewReader(pkt)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if recs[0].VarIOs != nil {
		t.Errorf("Codec 8 record carried VarIOs: %v", recs[0].VarIOs)
	}
}
