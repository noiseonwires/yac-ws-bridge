// Package quictun provides a virtual net.PacketConn that tunnels QUIC packets
// over the WebSocket bridge. Instead of real UDP, QUIC packets are sent via
// wsSend (gRPC API) or relayed through the cloud function, depending on config.
//
// Performance-critical design:
//   - WriteTo is non-blocking (pushes to channel).
//   - sendLoop batches multiple QUIC packets into one wsSend call, reducing
//     API call count by 5-20x. Batch is flushed on a short timer (default 5ms)
//     or when the batch reaches a size threshold.
//   - Backpressure (200ms) instead of silent drops to avoid cwnd collapse.
package quictun

import (
	"encoding/binary"
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// SendFunc sends a raw payload to the remote peer via the bridge.
// For batched mode, the payload is a batch-encoded blob of multiple packets.
type SendFunc func(data []byte) error

// BatchFlushDelay is how long to wait for more packets before flushing a batch.
const BatchFlushDelay = 5 * time.Millisecond

// MaxBatchSize is the maximum batch payload size before forcing a flush.
// Kept under 128KB wsSend limit with room for protocol framing.
const MaxBatchSize = 120 * 1024

// Transport is a virtual net.PacketConn backed by the WebSocket bridge.
type Transport struct {
	localAddr net.Addr
	peerAddr  net.Addr

	mu       sync.Mutex
	closed   bool
	readCh   chan []byte // incoming QUIC packets
	closeCh  chan struct{}
	deadline time.Time

	// Async send: WriteTo pushes packets here, sendLoop batches and sends.
	sendCh      chan []byte
	sendWorkers int
	send        SendFunc

	quicOnce      sync.Once
	quicTransport *quic.Transport
}

// NewTransport creates a virtual packet connection with batched async sending.
// sendWorkers controls how many concurrent batch-send goroutines run (default 4).
func NewTransport(sendFn SendFunc, sendWorkers int) *Transport {
	if sendWorkers <= 0 {
		sendWorkers = 4
	}
	t := &Transport{
		localAddr:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		peerAddr:    &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1},
		readCh:      make(chan []byte, 4096),
		closeCh:     make(chan struct{}),
		sendCh:      make(chan []byte, 4096),
		sendWorkers: sendWorkers,
		send:        sendFn,
	}
	for i := 0; i < sendWorkers; i++ {
		go t.sendLoop()
	}
	return t
}

// sendLoop collects packets from sendCh, batches them, and flushes.
// Strategy: send the first packet immediately (no delay), then collect any
// additional packets that arrive within BatchFlushDelay into a batch.
// This ensures low-latency for handshakes and control packets (which arrive
// one at a time), while still batching bulk data transfers (which arrive in bursts).
func (t *Transport) sendLoop() {
	var batch [][]byte
	var batchSize int
	timer := time.NewTimer(BatchFlushDelay)
	timer.Stop()
	timerRunning := false

	flush := func() {
		if len(batch) == 0 {
			return
		}
		payload := encodeBatchPayload(batch)
		if err := t.send(payload); err != nil {
			log.Printf("[DEBUG] transport send: %v", err)
		}
		batch = batch[:0]
		batchSize = 0
		if timerRunning {
			timer.Stop()
			timerRunning = false
		}
	}

	for {
		// Block until the first packet arrives.
		var first []byte
		select {
		case <-t.closeCh:
			flush()
			return
		case first = <-t.sendCh:
		}

		batch = append(batch, first)
		batchSize = 2 + len(first)

		// Non-blocking: drain any additional packets already queued.
		draining := true
		for draining {
			select {
			case pkt := <-t.sendCh:
				batch = append(batch, pkt)
				batchSize += 2 + len(pkt)
				if batchSize >= MaxBatchSize {
					flush()
					draining = false
				}
			default:
				draining = false
			}
		}

		// If we already flushed due to MaxBatchSize, continue.
		if len(batch) == 0 {
			continue
		}

		// If only 1 packet (common for handshake/ACKs), send immediately.
		if len(batch) == 1 {
			flush()
			continue
		}

		// Multiple packets queued — start timer to collect a few more.
		timer.Reset(BatchFlushDelay)
		timerRunning = true

	collectMore:
		for {
			select {
			case <-t.closeCh:
				flush()
				return
			case pkt := <-t.sendCh:
				batch = append(batch, pkt)
				batchSize += 2 + len(pkt)
				if batchSize >= MaxBatchSize {
					flush()
					break collectMore
				}
			case <-timer.C:
				timerRunning = false
				flush()
				break collectMore
			}
		}
	}
}

// encodeBatchPayload encodes multiple packets with length-prefix framing.
// Format: [2B len][packet][2B len][packet]...
func encodeBatchPayload(packets [][]byte) []byte {
	size := 0
	for _, p := range packets {
		size += 2 + len(p)
	}
	buf := make([]byte, size)
	off := 0
	for _, p := range packets {
		binary.BigEndian.PutUint16(buf[off:], uint16(len(p)))
		off += 2
		copy(buf[off:], p)
		off += len(p)
	}
	return buf
}

// Deliver enqueues an incoming QUIC packet from the bridge for the local QUIC stack.
// Makes a copy of data since the caller may reuse the buffer.
func (t *Transport) Deliver(data []byte) {
	buf := make([]byte, len(data))
	copy(buf, data)
	select {
	case t.readCh <- buf:
	default:
	}
}

// DeliverBatch splits a batch payload and delivers each packet individually.
func (t *Transport) DeliverBatch(data []byte) {
	off := 0
	for off+2 <= len(data) {
		pLen := int(binary.BigEndian.Uint16(data[off:]))
		off += 2
		if off+pLen > len(data) {
			break
		}
		buf := make([]byte, pLen)
		copy(buf, data[off:off+pLen])
		select {
		case t.readCh <- buf:
		default:
		}
		off += pLen
	}
}

// ReadFrom reads the next incoming QUIC packet.
func (t *Transport) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	t.mu.Lock()
	dl := t.deadline
	t.mu.Unlock()

	var tmr <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return 0, nil, &timeoutError{}
		}
		tm := time.NewTimer(d)
		defer tm.Stop()
		tmr = tm.C
	}

	select {
	case <-t.closeCh:
		return 0, nil, net.ErrClosed
	case data := <-t.readCh:
		n = copy(p, data)
		return n, t.peerAddr, nil
	case <-tmr:
		return 0, nil, &timeoutError{}
	}
}

// WriteTo queues a QUIC packet for async batched sending.
// Uses short-timeout backpressure instead of silent drops.
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
		return len(p), nil
	default:
	}
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case t.sendCh <- buf:
		return len(p), nil
	case <-timer.C:
		log.Printf("[WARN] send queue full after backpressure, dropping packet len=%d", len(buf))
		return len(p), nil
	case <-t.closeCh:
		return 0, net.ErrClosed
	}
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

func (t *Transport) LocalAddr() net.Addr            { return t.localAddr }
func (t *Transport) PeerAddr() net.Addr              { return t.peerAddr }
func (t *Transport) SetWriteDeadline(time.Time) error { return nil }

func (t *Transport) SetDeadline(tm time.Time) error {
	t.mu.Lock()
	t.deadline = tm
	t.mu.Unlock()
	return nil
}

func (t *Transport) SetReadDeadline(tm time.Time) error {
	return t.SetDeadline(tm)
}

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
