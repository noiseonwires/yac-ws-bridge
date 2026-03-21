package streams

import (
	"log"
	"net"
	"sync"
	"sync/atomic"

	"github.com/bridge-to-freedom/adapter/internal/protocol"
)

// SendFunc sends a protocol frame to the peer. Implementations differ between
// adapter (always wsSend) and helper (wsSend or relay via upstream WS).
type SendFunc func(data []byte) error

// Stream represents one multiplexed TCP connection.
type Stream struct {
	ID   uint32
	Conn net.Conn

	mu       sync.Mutex
	closed   bool
	halfOpen bool // received FIN but not yet closed
}

// Manager tracks active streams and dispatches incoming frames.
type Manager struct {
	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  atomic.Uint32 // helper-only: allocates stream IDs
	send    SendFunc
}

func NewManager(send SendFunc) *Manager {
	m := &Manager{
		streams: make(map[uint32]*Stream),
		send:    send,
	}
	m.nextID.Store(1)
	return m
}

// NextID allocates a new stream ID (used by helper).
func (m *Manager) NextID() uint32 {
	return m.nextID.Add(1) - 1
}

// Register adds a stream to the manager.
func (m *Manager) Register(s *Stream) {
	m.mu.Lock()
	m.streams[s.ID] = s
	m.mu.Unlock()
}

// Get returns a stream by ID, or nil.
func (m *Manager) Get(id uint32) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id]
}

// Remove unregisters a stream.
func (m *Manager) Remove(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
}

// SendFrame encodes and sends a frame to the peer.
func (m *Manager) SendFrame(f protocol.Frame) error {
	return m.send(protocol.Encode(f))
}

// HandleData writes payload to the stream's TCP connection.
func (m *Manager) HandleData(streamID uint32, payload []byte) {
	s := m.Get(streamID)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if _, err := s.Conn.Write(payload); err != nil {
		log.Printf("[WARN] write to TCP failed stream=%d err=%v", streamID, err)
	}
}

// HandleFin processes a graceful close from the peer.
func (m *Manager) HandleFin(streamID uint32) {
	s := m.Get(streamID)
	if s == nil {
		log.Printf("[DEBUG] FIN for unknown stream=%d", streamID)
		return
	}
	log.Printf("[DEBUG] FIN handling stream=%d", streamID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.halfOpen = true
	if tc, ok := s.Conn.(*net.TCPConn); ok {
		tc.CloseRead()
	}
}

// HandleRst aborts a stream immediately.
func (m *Manager) HandleRst(streamID uint32) {
	s := m.Get(streamID)
	if s == nil {
		log.Printf("[DEBUG] RST for unknown stream=%d", streamID)
		return
	}
	log.Printf("[DEBUG] RST handling stream=%d", streamID)
	m.CloseStream(s)
}

// CloseStream closes the TCP connection and removes the stream.
func (m *Manager) CloseStream(s *Stream) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	log.Printf("[DEBUG] closing stream=%d", s.ID)
	s.Conn.Close()
	m.Remove(s.ID)
}

// CloseAll RSTs all active streams and closes their TCP connections.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		all = append(all, s)
	}
	m.mu.Unlock()

	if len(all) > 0 {
		log.Printf("[INFO] closing all %d streams", len(all))
	}
	for _, s := range all {
		m.CloseStream(s)
	}
}

// ReadLoop reads from TCP and sends DATA frames to the peer.
// On EOF it sends FIN; on error it sends RST. Returns when done.
func (m *Manager) ReadLoop(s *Stream) {
	buf := make([]byte, 32*1024)
	defer func() {
		s.mu.Lock()
		wasClosed := s.closed
		s.mu.Unlock()
		if !wasClosed {
			log.Printf("[DEBUG] TCP read ended stream=%d, sending FIN", s.ID)
			m.SendFrame(protocol.Frame{Type: protocol.MsgFin, StreamID: s.ID})
			m.CloseStream(s)
		}
	}()

	for {
		n, err := s.Conn.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			if sendErr := m.SendFrame(protocol.Frame{
				Type:     protocol.MsgData,
				StreamID: s.ID,
				Payload:  payload,
			}); sendErr != nil {
				log.Printf("[WARN] send DATA failed stream=%d err=%v", s.ID, sendErr)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Count returns the number of active streams.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}
