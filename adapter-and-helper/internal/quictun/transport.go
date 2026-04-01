// Package quictun provides a virtual net.PacketConn that tunnels QUIC packets
// over the WebSocket bridge. Instead of real UDP, QUIC packets are sent via
// wsSend (gRPC API) or relayed through the cloud function, depending on config.
//
// Performance-critical: WriteTo is made non-blocking by using an async send
// queue. This prevents QUIC's congestion controller from misinterpreting
// gRPC/REST call latency as network RTT.
package quictun

import (
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// SendFunc sends a raw QUIC packet to the remote peer via the bridge.
type SendFunc func(data []byte) error

// Transport is a virtual net.PacketConn backed by the WebSocket bridge.
// The QUIC stack reads/writes from this instead of a real UDP socket.
type Transport struct {
	localAddr net.Addr
	peerAddr  net.Addr

	mu       sync.Mutex
	closed   bool
	readCh   chan []byte // incoming QUIC packets
	closeCh  chan struct{}
	deadline time.Time

	// Async send queue: WriteTo pushes packets here, background workers
	// drain and send via the bridge. This decouples QUIC pacing from
	// wsSend latency so the congestion controller sees sub-ms "RTT".
	sendCh      chan []byte
	sendWorkers int
	send        SendFunc

	// quic.Transport is created once and reused across Listen/Dial calls.
	quicOnce      sync.Once
	quicTransport *quic.Transport
}

// NewTransport creates a virtual packet connection with async sending.
// sendWorkers controls concurrency of outbound wsSend calls (default 8).
func NewTransport(sendFn SendFunc, sendWorkers int) *Transport {
	if sendWorkers <= 0 {
		sendWorkers = 8
	}
	t := &Transport{
		localAddr:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		peerAddr:    &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1},
		readCh:      make(chan []byte, 2048),
		closeCh:     make(chan struct{}),
		sendCh:      make(chan []byte, 2048),
		sendWorkers: sendWorkers,
		send:        sendFn,
	}
	// Start send workers.
	for i := 0; i < sendWorkers; i++ {
		go t.sendLoop()
	}
	return t
}

func (t *Transport) sendLoop() {
	for {
		select {
		case <-t.closeCh:
			return
		case pkt := <-t.sendCh:
			if err := t.send(pkt); err != nil {
				log.Printf("[DEBUG] transport send: %v", err)
			}
		}
	}
}

// Deliver enqueues an incoming QUIC packet from the bridge for the local QUIC stack.
func (t *Transport) Deliver(data []byte) {
	// Non-blocking: drop if buffer is full (QUIC handles retransmission).
	select {
	case t.readCh <- data:
	default:
	}
}

// ReadFrom reads the next incoming QUIC packet.
func (t *Transport) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	t.mu.Lock()
	dl := t.deadline
	t.mu.Unlock()

	var timer <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return 0, nil, &timeoutError{}
		}
		tm := time.NewTimer(d)
		defer tm.Stop()
		timer = tm.C
	}

	select {
	case <-t.closeCh:
		return 0, nil, net.ErrClosed
	case data := <-t.readCh:
		n = copy(p, data)
		return n, t.peerAddr, nil
	case <-timer:
		return 0, nil, &timeoutError{}
	}
}

// WriteTo queues a QUIC packet for async sending through the bridge.
// Returns immediately so QUIC's congestion controller is not blocked
// by wsSend latency. If the send queue is full, the packet is dropped
// (QUIC will retransmit).
func (t *Transport) WriteTo(p []byte, _ net.Addr) (n int, err error) {
	select {
	case <-t.closeCh:
		return 0, net.ErrClosed
	default:
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case t.sendCh <- buf:
	default:
		// Send queue full — drop. QUIC retransmits.
		log.Printf("[DEBUG] send queue full, dropping packet len=%d", len(buf))
	}
	return len(p), nil
}

// Close shuts down the transport.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.closeCh)
	}
	return nil
}

// LocalAddr returns a synthetic local address.
func (t *Transport) LocalAddr() net.Addr { return t.localAddr }

// SetDeadline sets the read deadline.
func (t *Transport) SetDeadline(tm time.Time) error {
	t.mu.Lock()
	t.deadline = tm
	t.mu.Unlock()
	return nil
}

// SetReadDeadline sets the read deadline.
func (t *Transport) SetReadDeadline(tm time.Time) error {
	return t.SetDeadline(tm)
}

// SetWriteDeadline is a no-op (writes are non-blocking sends).
func (t *Transport) SetWriteDeadline(time.Time) error { return nil }

// PeerAddr returns the synthetic remote address used for the QUIC peer.
func (t *Transport) PeerAddr() net.Addr { return t.peerAddr }

// getOrCreateQUICTransport returns the singleton quic.Transport wrapping this PacketConn.
func (t *Transport) getOrCreateQUICTransport() *quic.Transport {
	t.quicOnce.Do(func() {
		t.quicTransport = &quic.Transport{Conn: t}
	})
	return t.quicTransport
}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }
