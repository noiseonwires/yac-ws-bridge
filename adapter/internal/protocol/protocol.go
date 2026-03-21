package protocol

import (
	"encoding/binary"
	"errors"
)

// Message type constants.
const (
	MsgHello              byte = 0x01
	MsgHelloOK            byte = 0x02
	MsgHelloErr           byte = 0x03
	MsgClientConnected    byte = 0x10
	MsgClientDisconnected byte = 0x11
	MsgTargetConnected    byte = 0x12
	MsgTargetConnFailed   byte = 0x13
	MsgTargetDisconnected byte = 0x14
	MsgDataC2T            byte = 0x20
	MsgDataT2C            byte = 0x21
	MsgPing               byte = 0xF0
	MsgPong               byte = 0xF1

	FlagTextFrame byte = 0x01
)

// Frame is a single protocol message on the upstream connection.
// ClientID is a variable-length string (e.g. API Gateway connectionId).
// Empty string for control messages (HELLO, PING, PONG).
//
// Wire format:
//   [ClientID Len: 2 bytes uint16 BE] [ClientID: variable UTF-8] [Type: 1] [Flags: 1] [Payload: variable]
type Frame struct {
	ClientID string
	Type     byte
	Flags    byte
	Payload  []byte
}

// Encode serialises a Frame into a byte slice.
func Encode(f Frame) []byte {
	cidBytes := []byte(f.ClientID)
	buf := make([]byte, 2+len(cidBytes)+2+len(f.Payload))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(cidBytes)))
	copy(buf[2:], cidBytes)
	off := 2 + len(cidBytes)
	buf[off] = f.Type
	buf[off+1] = f.Flags
	copy(buf[off+2:], f.Payload)
	return buf
}

// EncodeBatch wraps multiple encoded frames with 4-byte length prefixes.
// Wire: [len1:4][frame1][len2:4][frame2]...
func EncodeBatch(frames [][]byte) []byte {
	total := 0
	for _, f := range frames {
		total += 4 + len(f)
	}
	buf := make([]byte, 0, total)
	for _, f := range frames {
		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(f)))
		buf = append(buf, lenBuf...)
		buf = append(buf, f...)
	}
	return buf
}

// DecodeBatch splits a length-prefixed batch into individual frame byte slices.
func DecodeBatch(data []byte) ([][]byte, error) {
	var frames [][]byte
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, errors.New("batch: short length prefix")
		}
		fLen := binary.BigEndian.Uint32(data[0:4])
		data = data[4:]
		if uint32(len(data)) < fLen {
			return nil, errors.New("batch: frame truncated")
		}
		frames = append(frames, data[:fLen])
		data = data[fLen:]
	}
	return frames, nil
}

// Decode parses a byte slice into a Frame.
func Decode(data []byte) (Frame, error) {
	if len(data) < 4 {
		return Frame{}, errors.New("frame too short")
	}
	cidLen := binary.BigEndian.Uint16(data[0:2])
	if int(2+cidLen+2) > len(data) {
		return Frame{}, errors.New("frame too short for client ID")
	}
	clientID := string(data[2 : 2+cidLen])
	off := 2 + int(cidLen)
	return Frame{
		ClientID: clientID,
		Type:     data[off],
		Flags:    data[off+1],
		Payload:  data[off+2:],
	}, nil
}

// ClientConnectedPayload holds the parsed payload of a CLIENT_CONNECTED message.
type ClientConnectedPayload struct {
	Path         string
	Subprotocols string
	IAMToken     string
}

// DecodeClientConnected parses the CLIENT_CONNECTED payload.
// Wire: [2B pathLen][path][2B subLen][sub][2B tokenLen][token]
func DecodeClientConnected(payload []byte) (ClientConnectedPayload, error) {
	if len(payload) < 4 {
		return ClientConnectedPayload{}, errors.New("payload too short")
	}
	pathLen := binary.BigEndian.Uint16(payload[0:2])
	if int(2+pathLen+2) > len(payload) {
		return ClientConnectedPayload{}, errors.New("invalid path length")
	}
	path := string(payload[2 : 2+pathLen])
	subOff := 2 + pathLen
	subLen := binary.BigEndian.Uint16(payload[subOff : subOff+2])
	if int(subOff+2+subLen) > len(payload) {
		return ClientConnectedPayload{}, errors.New("invalid subprotocol length")
	}
	sub := string(payload[subOff+2 : subOff+2+subLen])

	// IAM token (optional, for v3)
	var token string
	tokOff := subOff + 2 + subLen
	if tokOff+2 <= uint16(len(payload)) {
		tokLen := binary.BigEndian.Uint16(payload[tokOff : tokOff+2])
		if int(tokOff+2+tokLen) <= len(payload) {
			token = string(payload[tokOff+2 : tokOff+2+tokLen])
		}
	}

	return ClientConnectedPayload{Path: path, Subprotocols: sub, IAMToken: token}, nil
}
