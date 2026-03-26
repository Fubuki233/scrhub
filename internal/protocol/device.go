package protocol

import (
	"encoding/binary"
	"io"
)

// Device message types matching device_msg.h
const (
	DeviceMsgTypeClipboard    = 0
	DeviceMsgTypeAckClipboard = 1
	DeviceMsgTypeUHIDOutput   = 2
)

// ReadDeviceMsg reads a device message from the control socket.
// Returns the raw bytes for relaying to web clients.
func ReadDeviceMsg(r io.Reader) ([]byte, error) {
	var typeBuf [1]byte
	if _, err := io.ReadFull(r, typeBuf[:]); err != nil {
		return nil, err
	}

	msgType := typeBuf[0]
	switch msgType {
	case DeviceMsgTypeClipboard:
		// type(1) + text_len(4) + text(variable)
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return nil, err
		}
		textLen := binary.BigEndian.Uint32(lenBuf[:])
		if textLen > ControlMsgClipboardMaxLength {
			return nil, io.ErrUnexpectedEOF
		}
		text := make([]byte, textLen)
		if textLen > 0 {
			if _, err := io.ReadFull(r, text); err != nil {
				return nil, err
			}
		}
		raw := make([]byte, 5+textLen)
		raw[0] = msgType
		copy(raw[1:5], lenBuf[:])
		copy(raw[5:], text)
		return raw, nil

	case DeviceMsgTypeAckClipboard:
		// type(1) + sequence(8)
		var seqBuf [8]byte
		if _, err := io.ReadFull(r, seqBuf[:]); err != nil {
			return nil, err
		}
		raw := make([]byte, 9)
		raw[0] = msgType
		copy(raw[1:], seqBuf[:])
		return raw, nil

	case DeviceMsgTypeUHIDOutput:
		// type(1) + id(2) + size(2) + data(variable)
		var header [4]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, err
		}
		size := binary.BigEndian.Uint16(header[2:4])
		data := make([]byte, size)
		if size > 0 {
			if _, err := io.ReadFull(r, data); err != nil {
				return nil, err
			}
		}
		raw := make([]byte, 5+int(size))
		raw[0] = msgType
		copy(raw[1:5], header[:])
		copy(raw[5:], data)
		return raw, nil

	default:
		return nil, io.ErrUnexpectedEOF
	}
}
