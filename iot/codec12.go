package iot

// Teltonika Codec 12 — server→device GPRS commands.
//
// Spec reference: https://wiki.teltonika-gps.com/view/Codec#Codec_12
//
//	[4 bytes  preamble = 0x00000000]
//	[4 bytes  data size, counting everything from codec ID to the last command byte]
//	[1 byte   codec ID = 0x0C]
//	[1 byte   command quantity 1]
//	[1 byte   type: 0x05 = command, 0x06 = response]
//	[4 bytes  command size]
//	[N bytes  command text, ASCII]
//	[1 byte   command quantity 2, must equal quantity 1]
//	[4 bytes  CRC-16/IBM over the data field, low two bytes]
//
// A deliberate distinction from the SinoTrack path: this ENVELOPE is a
// published, deterministic spec and reuses the same CRC-16/IBM already verified
// against real Codec 8 packets, so it can be implemented correctly here. What
// still cannot be guessed is the command TEXT and which digital output a given
// installation wired the relay to — "setdigout 1" only immobilises a truck if
// someone actually connected DOUT1 to the ignition relay.
//
// So framing lives here, and the command string stays behind the per-model
// encoder registry. Nothing reaches a device until an installation is confirmed.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// CodecID12 identifies a GPRS command/response frame.
	CodecID12 byte = 0x0C

	codec12TypeCommand  byte = 0x05
	codec12TypeResponse byte = 0x06

	// maxCodec12Command bounds the command text. Real commands are short
	// ("setdigout 1"); anything larger indicates a caller bug and must not be
	// framed and sent to a vehicle.
	maxCodec12Command = 512
)

var (
	ErrCodec12Empty    = errors.New("codec12: empty command")
	ErrCodec12TooLong  = errors.New("codec12: command exceeds maximum length")
	ErrCodec12BadFrame = errors.New("codec12: malformed frame")
)

// BuildCodec12Command frames an ASCII command for transmission to a device.
func BuildCodec12Command(command string) ([]byte, error) {
	if command == "" {
		return nil, ErrCodec12Empty
	}
	if len(command) > maxCodec12Command {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrCodec12TooLong, len(command), maxCodec12Command)
	}

	// Data field: codec ID, quantity, type, size, payload, quantity again.
	data := make([]byte, 0, 11+len(command))
	data = append(data, CodecID12, 0x01, codec12TypeCommand)
	var sizeBuf [4]byte
	binary.BigEndian.PutUint32(sizeBuf[:], uint32(len(command)))
	data = append(data, sizeBuf[:]...)
	data = append(data, command...)
	data = append(data, 0x01)

	out := make([]byte, 0, 8+len(data)+4)
	out = append(out, 0x00, 0x00, 0x00, 0x00) // preamble
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	out = append(out, lenBuf[:]...)
	out = append(out, data...)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], uint32(crc16IBM(data)))
	out = append(out, crcBuf[:]...)
	return out, nil
}

// ParseCodec12Response decodes a device's reply to a command, so an operator
// can be told whether the unit actually accepted it rather than only that the
// bytes left the server.
func ParseCodec12Response(frame []byte) (string, error) {
	if len(frame) < 16 {
		return "", fmt.Errorf("%w: %d bytes is too short", ErrCodec12BadFrame, len(frame))
	}
	if binary.BigEndian.Uint32(frame[0:4]) != 0 {
		return "", fmt.Errorf("%w: bad preamble", ErrCodec12BadFrame)
	}
	dataLen := int(binary.BigEndian.Uint32(frame[4:8]))
	if dataLen <= 0 || 8+dataLen+4 > len(frame) {
		return "", fmt.Errorf("%w: data size %d does not fit the frame", ErrCodec12BadFrame, dataLen)
	}
	data := frame[8 : 8+dataLen]
	gotCRC := binary.BigEndian.Uint32(frame[8+dataLen:8+dataLen+4]) & 0xFFFF
	if wantCRC := uint32(crc16IBM(data)); gotCRC != wantCRC {
		return "", fmt.Errorf("%w: CRC got 0x%04X want 0x%04X", ErrCodec12BadFrame, gotCRC, wantCRC)
	}
	if data[0] != CodecID12 {
		return "", fmt.Errorf("%w: codec 0x%02X is not Codec 12", ErrCodec12BadFrame, data[0])
	}
	if len(data) < 8 || data[2] != codec12TypeResponse {
		return "", fmt.Errorf("%w: not a response frame", ErrCodec12BadFrame)
	}
	respLen := int(binary.BigEndian.Uint32(data[3:7]))
	if respLen < 0 || 7+respLen > len(data) {
		return "", fmt.Errorf("%w: response size %d does not fit", ErrCodec12BadFrame, respLen)
	}
	return string(data[7 : 7+respLen]), nil
}
