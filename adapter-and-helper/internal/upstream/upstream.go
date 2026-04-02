package upstream

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/bridge-to-freedom/adapter/internal/config"
	"github.com/bridge-to-freedom/adapter/internal/protocol"
	"github.com/gorilla/websocket"
)

// FrameHandler is called for each decoded frame from the upstream WS.
type FrameHandler func(f protocol.Frame)

// Upstream manages a persistent WebSocket connection to the API Gateway.
type Upstream struct {
	cfg     *config.Config
	handler FrameHandler

	mu          sync.Mutex
	writeMu     sync.Mutex // serializes all WebSocket writes
	conn        *websocket.Conn
	running     bool
	ownConnID   string
	peerConnID  string
	staleConnID string // last peer ID that failed wsSend
	iamToken    string
}

func New(cfg *config.Config, handler FrameHandler) *Upstream {
	return &Upstream{cfg: cfg, handler: handler}
}

// OwnConnID returns this side's upstream connection ID.
func (u *Upstream) OwnConnID() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.ownConnID
}

// PeerConnID returns the peer's upstream connection ID.
func (u *Upstream) PeerConnID() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.peerConnID
}

// SetPeerConnID updates the peer connection ID (from PEER_CONN).
func (u *Upstream) SetPeerConnID(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.peerConnID = id
}

// IAMToken returns the current IAM token.
func (u *Upstream) IAMToken() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.iamToken
}

// SetIAMToken updates the IAM token (from PONG or PEER_CONN).
func (u *Upstream) SetIAMToken(token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.iamToken = token
}

// Send writes a binary frame to the upstream WS. Thread-safe.
func (u *Upstream) Send(data []byte) error {
	u.mu.Lock()
	c := u.conn
	u.mu.Unlock()
	if c == nil {
		return fmt.Errorf("upstream not connected")
	}
	u.writeMu.Lock()
	err := c.WriteMessage(websocket.BinaryMessage, data)
	u.writeMu.Unlock()
	return err
}

// SendSync sends a SYNC frame through the upstream WS.
func (u *Upstream) SendSync() error {
	return u.Send(protocol.Encode(protocol.Frame{Type: protocol.MsgSync}))
}

// MarkPeerStale clears the peer connection ID and triggers a SYNC.
// Call this when wsSend to the peer fails.
func (u *Upstream) MarkPeerStale() {
	u.mu.Lock()
	old := u.peerConnID
	u.peerConnID = ""
	u.staleConnID = old
	u.mu.Unlock()
	if old != "" {
		log.Printf("[WARN] peer connId %s marked stale, sending SYNC", old)
		if err := u.SendSync(); err != nil {
			log.Printf("[WARN] SYNC send failed: %v", err)
		}
	}
}

// IsStaleConnID returns true if the given ID was the last one marked stale.
// Used to reject PEER_CONN with the same broken ID.
func (u *Upstream) IsStaleConnID(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return id != "" && id == u.staleConnID
}

// ClearStaleConnID clears the stale ID tracking (call when a new valid peer connects).
func (u *Upstream) ClearStaleConnID() {
	u.mu.Lock()
	u.staleConnID = ""
	u.mu.Unlock()
}

func (u *Upstream) dial(ctx context.Context) (*websocket.Conn, error) {
	log.Printf("[INFO] connecting to %s ...", u.cfg.Bridge.URL)
	dialer := websocket.Dialer{
		EnableCompression: false,
		HandshakeTimeout:  10 * time.Second,
	}
	ws, httpResp, err := dialer.DialContext(ctx, u.cfg.Bridge.URL, nil)
	if err != nil {
		if httpResp != nil {
			log.Printf("[WARN] dial failed: %v (HTTP status %d)", err, httpResp.StatusCode)
		}
		return nil, err
	}
	log.Printf("[INFO] WebSocket connected, sending HELLO...")

	// HELLO
	hello := protocol.Encode(protocol.Frame{
		Type:    protocol.MsgHello,
		Payload: protocol.EncodeHello(0x01, u.cfg.Bridge.AuthToken),
	})
	if err := ws.WriteMessage(websocket.BinaryMessage, hello); err != nil {
		ws.Close()
		return nil, fmt.Errorf("send HELLO: %w", err)
	}

	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := ws.ReadMessage()
	ws.SetReadDeadline(time.Time{}) // clear deadline
	if err != nil {
		ws.Close()
		return nil, fmt.Errorf("read HELLO response (timeout?): %w", err)
	}
	resp, err := protocol.Decode(msg)
	if err != nil {
		ws.Close()
		return nil, err
	}
	if resp.Type == protocol.MsgHelloErr {
		ws.Close()
		return nil, fmt.Errorf("rejected: %s", string(resp.Payload))
	}
	if resp.Type != protocol.MsgHelloOK {
		ws.Close()
		return nil, fmt.Errorf("unexpected response: 0x%02x", resp.Type)
	}

	ownID, peerID, iamToken, err := protocol.DecodeHelloOK(resp.Payload)
	if err != nil {
		ws.Close()
		return nil, fmt.Errorf("decode HELLO_OK: %w", err)
	}

	u.mu.Lock()
	u.ownConnID = ownID
	u.peerConnID = peerID
	u.iamToken = iamToken
	u.mu.Unlock()

	log.Printf("[INFO] upstream authenticated ownID=%s peerID=%s", ownID, peerID)

	if tc, ok := ws.UnderlyingConn().(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return ws, nil
}

func (u *Upstream) readLoop(ctx context.Context) {
	if pi := u.cfg.PingInterval(); pi > 0 {
		go u.pingLoop(ctx, pi)
	}
	for {
		msgType, msg, err := u.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[WARN] upstream read error: %v", err)
			return
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		f, err := protocol.Decode(msg)
		if err != nil {
			log.Printf("[DEBUG] bad upstream frame: %v", err)
			continue
		}
		u.handler(f)
	}
}

func (u *Upstream) pingLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.mu.Lock()
			c := u.conn
			u.mu.Unlock()
			if c != nil {
				log.Printf("[DEBUG] sending PING")
				f := protocol.Encode(protocol.Frame{Type: protocol.MsgPing})
				u.writeMu.Lock()
				c.WriteMessage(websocket.BinaryMessage, f)
				u.writeMu.Unlock()
			}
		}
	}
}

// Run connects and reconnects in a loop until ctx is cancelled.
func (u *Upstream) Run(ctx context.Context) {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return
	}
	u.running = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	delay := u.cfg.InitialDelay()
	for {
		if ctx.Err() != nil {
			return
		}
		ws, err := u.dial(ctx)
		if err != nil {
			log.Printf("[WARN] connect failed: %v retryIn=%v", err, delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			next := time.Duration(float64(delay) * u.cfg.Bridge.Reconnect.BackoffMultiplier)
			if next > u.cfg.MaxDelay() {
				next = u.cfg.MaxDelay()
			}
			delay = next
			continue
		}
		delay = u.cfg.InitialDelay()

		u.mu.Lock()
		u.conn = ws
		u.mu.Unlock()
		log.Println("[INFO] upstream connected")

		// Send an immediate PING to force the cloud function to cross-notify
		// the peer of our connId. Without this, discovery depends on the
		// periodic PING (30s default) and the peer may time out waiting.
		{
			f := protocol.Encode(protocol.Frame{Type: protocol.MsgPing})
			u.writeMu.Lock()
			ws.WriteMessage(websocket.BinaryMessage, f)
			u.writeMu.Unlock()
			log.Println("[DEBUG] sent initial PING for fast discovery")
		}

		readCtx, readCancel := context.WithCancel(ctx)
		captured := ws
		go func() {
			<-readCtx.Done()
			// Try graceful WS close, then force-close quickly
			closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown")
			captured.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(500*time.Millisecond))
			time.Sleep(200 * time.Millisecond)
			captured.Close()
		}()

		u.readLoop(readCtx)
		readCancel()

		u.mu.Lock()
		u.conn = nil
		u.ownConnID = ""
		u.peerConnID = ""
		u.iamToken = ""
		u.mu.Unlock()

		if ctx.Err() != nil {
			log.Println("[INFO] upstream shut down")
			return
		}
		log.Println("[INFO] upstream disconnected, reconnecting...")
	}
}
