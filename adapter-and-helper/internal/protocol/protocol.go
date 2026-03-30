package protocol

import (
	"encoding/binary"
	"errors"
)

// Message types — control (streamID = 0).
const (
	MsgHello    byte = 0x01
	MsgHelloOK  byte = 0x02
	MsgHelloErr byte = 0x03
	MsgPeerConn byte = 0x04
	MsgPeerGone byte = 0x05
	MsgSync     byte = 0x06
	MsgPing     byte = 0xF0
	MsgPong     byte = 0xF1
)

// Message types — stream (streamID > 0).
const (
	MsgOpen     byte = 0x10
	MsgOpenOK   byte = 0x11
	MsgOpenFail byte = 0x12
	MsgData     byte = 0x20
	MsgFin      byte = 0x21
	MsgRst      byte = 0x22
)

// Frame is a single protocol message.
// Wire: [1B type][4B streamID BE][4B seqID BE][payload...]
type Frame struct {
	Type     byte
	StreamID uint32
	SeqID    uint32
	Payload  []byte
}

// Encode serialises a Frame.
func Encode(f Frame) []byte {
	buf := make([]byte, 9+len(f.Payload))
	buf[0] = f.Type
	binary.BigEndian.PutUint32(buf[1:5], f.StreamID)
	binary.BigEndian.PutUint32(buf[5:9], f.SeqID)
	copy(buf[9:], f.Payload)
	return buf
}

// Decode parses a byte slice into a Frame.
func Decode(data []byte) (Frame, error) {
	if len(data) < 9 {
		return Frame{}, errors.New("frame too short")
	}
	return Frame{
		Type:     data[0],
		StreamID: binary.BigEndian.Uint32(data[1:5]),
		SeqID:    binary.BigEndian.Uint32(data[5:9]),
		Payload:  data[9:],
	}, nil
}

// EncodeHello builds a HELLO payload: [1B version][token UTF-8].
func EncodeHello(version byte, token string) []byte {
	t := []byte(token)
	buf := make([]byte, 1+len(t))
	buf[0] = version
	copy(buf[1:], t)
	return buf
}

// DecodeHello parses a HELLO payload.
func DecodeHello(payload []byte) (version byte, token string, err error) {
	if len(payload) < 1 {
		return 0, "", errors.New("HELLO payload too short")
	}
	return payload[0], string(payload[1:]), nil
}

// EncodeHelloOK builds a HELLO_OK payload:
// [2B ownIdLen][ownId][2B peerIdLen][peerId][2B tokenLen][iamToken]
func EncodeHelloOK(ownID, peerID, iamToken string) []byte {
	o := []byte(ownID)
	p := []byte(peerID)
	t := []byte(iamToken)
	buf := make([]byte, 2+len(o)+2+len(p)+2+len(t))
	off := 0
	binary.BigEndian.PutUint16(buf[off:], uint16(len(o)))
	off += 2
	copy(buf[off:], o)
	off += len(o)
	binary.BigEndian.PutUint16(buf[off:], uint16(len(p)))
	off += 2
	copy(buf[off:], p)
	off += len(p)
	binary.BigEndian.PutUint16(buf[off:], uint16(len(t)))
	off += 2
	copy(buf[off:], t)
	return buf
}

// DecodeHelloOK parses a HELLO_OK payload.
func DecodeHelloOK(payload []byte) (ownID, peerID, iamToken string, err error) {
	if len(payload) < 6 {
		return "", "", "", errors.New("HELLO_OK payload too short")
	}
	off := 0
	oLen := int(binary.BigEndian.Uint16(payload[off:]))
	off += 2
	if off+oLen+2 > len(payload) {
		return "", "", "", errors.New("HELLO_OK: bad ownID length")
	}
	ownID = string(payload[off : off+oLen])
	off += oLen

	pLen := int(binary.BigEndian.Uint16(payload[off:]))
	off += 2
	if off+pLen+2 > len(payload) {
		return "", "", "", errors.New("HELLO_OK: bad peerID length")
	}
	peerID = string(payload[off : off+pLen])
	off += pLen

	tLen := int(binary.BigEndian.Uint16(payload[off:]))
	off += 2
	if off+tLen > len(payload) {
		return "", "", "", errors.New("HELLO_OK: bad token length")
	}
	iamToken = string(payload[off : off+tLen])
	return
}

// EncodePeerConn builds a PEER_CONN payload:
// [2B peerIdLen][peerId][2B tokenLen][iamToken]
func EncodePeerConn(peerID, iamToken string) []byte {
	p := []byte(peerID)
	t := []byte(iamToken)
	buf := make([]byte, 2+len(p)+2+len(t))
	off := 0
	binary.BigEndian.PutUint16(buf[off:], uint16(len(p)))
	off += 2
	copy(buf[off:], p)
	off += len(p)
	binary.BigEndian.PutUint16(buf[off:], uint16(len(t)))
	off += 2
	copy(buf[off:], t)
	return buf
}

// DecodePeerConn parses a PEER_CONN payload.
func DecodePeerConn(payload []byte) (peerID, iamToken string, err error) {
	if len(payload) < 4 {
		return "", "", errors.New("PEER_CONN payload too short")
	}
	off := 0
	pLen := int(binary.BigEndian.Uint16(payload[off:]))
	off += 2
	if off+pLen+2 > len(payload) {
		return "", "", errors.New("PEER_CONN: bad peerID length")
	}
	peerID = string(payload[off : off+pLen])
	off += pLen

	tLen := int(binary.BigEndian.Uint16(payload[off:]))
	off += 2
	if off+tLen > len(payload) {
		return "", "", errors.New("PEER_CONN: bad token length")
	}
	iamToken = string(payload[off : off+tLen])
	return
}

// EncodePong builds a PONG payload: [2B tokenLen][iamToken].
func EncodePong(iamToken string) []byte {
	t := []byte(iamToken)
	buf := make([]byte, 2+len(t))
	binary.BigEndian.PutUint16(buf[0:], uint16(len(t)))
	copy(buf[2:], t)
	return buf
}

// DecodePong parses a PONG payload.
func DecodePong(payload []byte) (iamToken string, err error) {
	if len(payload) < 2 {
		return "", errors.New("PONG payload too short")
	}
	tLen := int(binary.BigEndian.Uint16(payload[0:]))
	if 2+tLen > len(payload) {
		return "", errors.New("PONG: bad token length")
	}
	return string(payload[2 : 2+tLen]), nil
}
