package streams

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

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
	mu             sync.Mutex
	streams        map[uint32]*Stream
	nextID         atomic.Uint32 // helper-only: allocates stream IDs
	send           SendFunc
	CoalesceDelay  time.Duration // 0 = disabled

	// Per-stream send sequence counters (auto-incremented in SendFrame).
	seqCounters sync.Map // streamID → *atomic.Uint32

	// Reorder incoming stream frames by SeqID (adapter-only).
	Reorder     bool
	reorderMu   sync.Mutex
	reorderBufs map[uint32]*reorderBuf
}

// reorderBuf holds out-of-order frames for a single stream.
type reorderBuf struct {
	mu       sync.Mutex
	expected uint32
	pending  map[uint32]protocol.Frame
}

func NewManager(send SendFunc) *Manager {
	m := &Manager{
		streams:     make(map[uint32]*Stream),
		send:        send,
		reorderBufs: make(map[uint32]*reorderBuf),
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

// Remove unregisters a stream and cleans up associated state.
func (m *Manager) Remove(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
	m.seqCounters.Delete(id)
	if m.Reorder {
		m.reorderMu.Lock()
		delete(m.reorderBufs, id)
		m.reorderMu.Unlock()
	}
}

// SendFrame encodes and sends a frame to the peer.
// For stream frames (StreamID > 0), SeqID is auto-assigned.
func (m *Manager) SendFrame(f protocol.Frame) error {
	if f.StreamID > 0 {
		v, _ := m.seqCounters.LoadOrStore(f.StreamID, &atomic.Uint32{})
		f.SeqID = v.(*atomic.Uint32).Add(1)
	}
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

	// Clear seq counters
	m.seqCounters.Range(func(key, _ any) bool {
		m.seqCounters.Delete(key)
		return true
	})
	// Clear reorder buffers
	if m.Reorder {
		m.reorderMu.Lock()
		m.reorderBufs = make(map[uint32]*reorderBuf)
		m.reorderMu.Unlock()
	}
}

// HandleStreamFrame processes an incoming stream frame with optional reordering.
// When Reorder is true, frames are buffered and delivered in SeqID order.
// The handler callback is invoked for each frame in sequence order and may be
// called multiple times if buffered frames become deliverable.
func (m *Manager) HandleStreamFrame(f protocol.Frame, handler func(protocol.Frame)) {
	if !m.Reorder || f.SeqID == 0 {
		handler(f)
		return
	}

	m.reorderMu.Lock()
	rb, ok := m.reorderBufs[f.StreamID]
	if !ok {
		rb = &reorderBuf{expected: 1, pending: make(map[uint32]protocol.Frame)}
		m.reorderBufs[f.StreamID] = rb
	}
	m.reorderMu.Unlock()

	rb.mu.Lock()
	defer rb.mu.Unlock()

	if f.SeqID == rb.expected {
		handler(f)
		rb.expected++
		// Drain consecutive buffered frames.
		for {
			next, exists := rb.pending[rb.expected]
			if !exists {
				break
			}
			delete(rb.pending, rb.expected)
			handler(next)
			rb.expected++
		}
	} else if f.SeqID > rb.expected {
		rb.pending[f.SeqID] = f
		if len(rb.pending)%100 == 0 {
			log.Printf("[WARN] reorder buffer growing stream=%d pending=%d expected=%d got=%d",
				f.StreamID, len(rb.pending), rb.expected, f.SeqID)
		}
	} else {
		log.Printf("[WARN] duplicate/old frame stream=%d seq=%d expected=%d", f.StreamID, f.SeqID, rb.expected)
	}
}

// ReadLoop reads from TCP and sends DATA frames to the peer.
// On EOF it sends FIN; on error it sends RST. Returns when done.
// When CoalesceDelay > 0, small reads are buffered and flushed as one
// DATA frame after the delay expires (Nagle-like write coalescing).
func (m *Manager) ReadLoop(s *Stream) {
	buf := make([]byte, 32*1024)
	var coalesceBuf []byte
	coalesce := m.CoalesceDelay > 0

	flush := func() {
		if len(coalesceBuf) == 0 {
			return
		}
		payload := coalesceBuf
		coalesceBuf = nil
		if sendErr := m.SendFrame(protocol.Frame{
			Type:     protocol.MsgData,
			StreamID: s.ID,
			Payload:  payload,
		}); sendErr != nil {
			log.Printf("[WARN] send DATA failed stream=%d err=%v", s.ID, sendErr)
		}
	}

	defer func() {
		if coalesce {
			flush()
		}
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
		// If coalescing and we have buffered data, set a short read deadline
		// so we flush after CoalesceDelay if no more data arrives.
		if coalesce && len(coalesceBuf) > 0 {
			s.Conn.SetReadDeadline(time.Now().Add(m.CoalesceDelay))
		} else if coalesce {
			s.Conn.SetReadDeadline(time.Time{}) // block indefinitely
		}

		n, err := s.Conn.Read(buf)
		if n > 0 {
			if coalesce {
				coalesceBuf = append(coalesceBuf, buf[:n]...)
				// Flush immediately if buffer is large enough
				if len(coalesceBuf) >= 32*1024 {
					flush()
				}
			} else {
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
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Read deadline expired — flush buffered data and continue
				flush()
				continue
			}
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
