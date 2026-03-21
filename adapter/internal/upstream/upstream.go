package upstream

import (
"context"
"fmt"
"log"
"net"
"sync"
"time"

"github.com/bridge-to-freedom/adapter/internal/config"
"github.com/bridge-to-freedom/adapter/internal/handler"
"github.com/bridge-to-freedom/adapter/internal/protocol"
"github.com/gorilla/websocket"
)

type Upstream struct {
cfg     *config.Config
handler *handler.Handler
mu      sync.Mutex
conn    *websocket.Conn
running bool
upstreamConnID string
}

func New(cfg *config.Config, h *handler.Handler) *Upstream {
return &Upstream{cfg: cfg, handler: h}
}

func (u *Upstream) dial(ctx context.Context) (*websocket.Conn, error) {
dialer := websocket.Dialer{EnableCompression: false}
ws, _, err := dialer.DialContext(ctx, u.cfg.Bridge.URL, nil)
if err != nil {
return nil, err
}

// HELLO
helloPayload := make([]byte, 1+len(u.cfg.Bridge.AuthToken))
helloPayload[0] = 0x01
copy(helloPayload[1:], []byte(u.cfg.Bridge.AuthToken))
frame := protocol.Encode(protocol.Frame{Type: protocol.MsgHello, Payload: helloPayload})
if err := ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
ws.Close()
return nil, fmt.Errorf("send HELLO: %w", err)
}

_, msg, err := ws.ReadMessage()
if err != nil {
ws.Close()
return nil, fmt.Errorf("read HELLO response: %w", err)
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
return nil, fmt.Errorf("unexpected: 0x%02x", resp.Type)
}

// Store the upstream connection ID returned by the bridge
u.mu.Lock()
u.upstreamConnID = string(resp.Payload)
u.mu.Unlock()
if u.upstreamConnID != "" {
log.Printf("[INFO] upstream connection ID: %s", u.upstreamConnID)
}

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
		log.Printf("[WARN] upstream read error err=%v", err)
return
}
if msgType != websocket.BinaryMessage {
		log.Printf("[WARN] non-binary upstream msg len=%d", len(msg))
continue
}
f, err := protocol.Decode(msg)
if err != nil {
		log.Printf("[DEBUG] bad upstream frame err=%v", err)
continue
}
u.handler.HandleFrame(f)
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
if u.conn != nil {
f := protocol.Encode(protocol.Frame{Type: protocol.MsgPing})
u.conn.WriteMessage(websocket.BinaryMessage, f)
}
u.mu.Unlock()
}
}
}

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
		log.Printf("[WARN] connect failed err=%v retryIn=%v", err, delay)
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
log.Println("[INFO] upstream connected and authenticated")

readCtx, readCancel := context.WithCancel(ctx)
captured := ws
go func() {
<-readCtx.Done()
// Graceful close: send CloseNormalClosure frame, then wait briefly for peer response
closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown")
captured.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(2*time.Second))
time.Sleep(500 * time.Millisecond)
captured.Close()
}()

u.readLoop(readCtx)
readCancel()

u.mu.Lock()
u.conn = nil
u.upstreamConnID = ""
u.mu.Unlock()
u.handler.CloseAll()

if ctx.Err() != nil {
return
}
	log.Println("[INFO] reconnecting...")
}
}

// UpstreamConnID returns the upstream WebSocket connection ID assigned by the bridge.
func (u *Upstream) UpstreamConnID() string {
u.mu.Lock()
defer u.mu.Unlock()
return u.upstreamConnID
}

// EnsureConnected starts the upstream loop if not already running.
func (u *Upstream) EnsureConnected(ctx context.Context) {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return
	}
	u.mu.Unlock()
	go u.Run(ctx)
}