package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Scrcpy protocol constants matching demuxer.c
const (
	PacketHeaderSize = 12

	PacketFlagConfig   = uint64(1) << 63
	PacketFlagKeyFrame = uint64(1) << 62
	PacketPTSMask      = PacketFlagKeyFrame - 1

	// Video codec IDs (ASCII as 32-bit big-endian)
	CodecH264 = uint32(0x68323634) // "h264"
	CodecH265 = uint32(0x68323635) // "h265"
	CodecAV1  = uint32(0x00617631) // "av1"

	// Audio codec IDs
	CodecOpus = uint32(0x6f707573) // "opus"
	CodecAAC  = uint32(0x00616163) // "aac"
	CodecFLAC = uint32(0x666c6163) // "flac"
	CodecRAW  = uint32(0x00726177) // "raw"

	// Special codec ID values
	CodecDisabled = uint32(0)
	CodecError    = uint32(1)

	// Device name field length (from server.h)
	DeviceNameFieldLength = 64
)

// StreamPacket is a parsed scrcpy media packet.
type StreamPacket struct {
	PTS       uint64
	IsConfig  bool
	IsKeyFrame bool
	Data      []byte
}

// ReadCodecID reads the 4-byte codec ID sent at start of each stream.
func ReadCodecID(r io.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, fmt.Errorf("read codec id: %w", err)
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

// ReadVideoSize reads the 8-byte video dimensions sent after codec ID on video stream.
func ReadVideoSize(r io.Reader) (width, height uint32, err error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, 0, fmt.Errorf("read video size: %w", err)
	}
	width = binary.BigEndian.Uint32(buf[:4])
	height = binary.BigEndian.Uint32(buf[4:])
	return width, height, nil
}

// ReadDeviceName reads the 64-byte device name from the first connected socket.
func ReadDeviceName(r io.Reader) (string, error) {
	buf := make([]byte, DeviceNameFieldLength)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("read device name: %w", err)
	}
	// Find null terminator
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}
	return string(buf), nil
}

// ReadPacket reads a single stream packet (12-byte header + payload).
// Returns the raw header+payload for relaying, plus the parsed packet for inspection.
func ReadPacket(r io.Reader) (*StreamPacket, []byte, error) {
	header := make([]byte, PacketHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, nil, err
	}

	ptsFlags := binary.BigEndian.Uint64(header[:8])
	payloadLen := binary.BigEndian.Uint32(header[8:12])

	if payloadLen == 0 {
		return nil, nil, fmt.Errorf("invalid zero-length packet")
	}
	if payloadLen > 4*1024*1024 { // sanity limit: 4MB
		return nil, nil, fmt.Errorf("packet too large: %d bytes", payloadLen)
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, nil, fmt.Errorf("read packet payload: %w", err)
	}

	pkt := &StreamPacket{
		IsConfig:   ptsFlags&PacketFlagConfig != 0,
		IsKeyFrame: ptsFlags&PacketFlagKeyFrame != 0,
		Data:       payload,
	}
	if pkt.IsConfig {
		pkt.PTS = 0
	} else {
		pkt.PTS = ptsFlags & PacketPTSMask
	}

	// Build raw frame = header + payload for efficient relay
	raw := make([]byte, PacketHeaderSize+int(payloadLen))
	copy(raw[:PacketHeaderSize], header)
	copy(raw[PacketHeaderSize:], payload)

	return pkt, raw, nil
}

// CodecName returns a human-readable name for a codec ID.
func CodecName(id uint32) string {
	switch id {
	case CodecH264:
		return "h264"
	case CodecH265:
		return "h265"
	case CodecAV1:
		return "av1"
	case CodecOpus:
		return "opus"
	case CodecAAC:
		return "aac"
	case CodecFLAC:
		return "flac"
	case CodecRAW:
		return "raw"
	default:
		return fmt.Sprintf("unknown(0x%08x)", id)
	}
}
