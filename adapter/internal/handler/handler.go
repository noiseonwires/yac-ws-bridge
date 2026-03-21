package handler

import (
"context"
"log"
"net"
"net/http"
"sync"
"time"

"github.com/bridge-to-freedom/adapter/internal/config"
"github.com/bridge-to-freedom/adapter/internal/protocol"
"github.com/bridge-to-freedom/adapter/internal/wsapi"
"github.com/gorilla/websocket"
)

type clientState struct {
targetWS *websocket.Conn
cancel   context.CancelFunc
mu       sync.Mutex
iamToken string
pending  []pendingMsg
}

type pendingMsg struct {
msgType int
data    []byte
}

type Handler struct {
mu      sync.Mutex
clients map[string]*clientState
cfg     *config.Config
ws      wsapi.Client
}

func New(cfg *config.Config) *Handler {
return &Handler{
clients: make(map[string]*clientState),
cfg: cfg,
ws: wsapi.NewClient(cfg.WsApi.Mode),
}
}

// HandleFrame processes a protocol frame from the upstream WS.
func (h *Handler) HandleFrame(f protocol.Frame) {
switch f.Type {
case protocol.MsgClientConnected:
h.onClientConnected(f)
case protocol.MsgDataC2T:
h.onDataC2T(f)
case protocol.MsgClientDisconnected:
h.onClientDisconnected(f)
case protocol.MsgPing:
// handled by upstream
case protocol.MsgPong:
// expected reply to our ping
default:
	log.Printf("[WARN] unknown frame type type=0x%02x", f.Type)
}
}

func (h *Handler) onClientConnected(f protocol.Frame) {
payload, err := protocol.DecodeClientConnected(f.Payload)
if err != nil {
	log.Printf("[ERROR] bad CLIENT_CONNECTED err=%v", err)
return
}

ctx, cancel := context.WithCancel(context.Background())
cs := &clientState{cancel: cancel, iamToken: payload.IAMToken}

h.mu.Lock()
h.clients[f.ClientID] = cs
h.mu.Unlock()

go h.connectToTarget(ctx, f.ClientID, payload, cs)
}

func (h *Handler) connectToTarget(ctx context.Context, clientID string, p protocol.ClientConnectedPayload, cs *clientState) {
targetURL := h.cfg.Target.URL + p.Path
start := time.Now()

dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, EnableCompression: false}
header := http.Header{}
if p.Subprotocols != "" {
// Set as raw header AND as dialer subprotocol to maximize compatibility
header["Sec-WebSocket-Protocol"] = []string{p.Subprotocols}
dialer.Subprotocols = []string{p.Subprotocols}
}

conn, resp, err := dialer.DialContext(ctx, targetURL, header)
if err != nil {
	log.Printf("[ERROR] target connect failed clientID=%s err=%v took=%v", clientID, err, time.Since(start))
if resp != nil {
	log.Printf("[ERROR] target response status=%d proto=%s", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Protocol"))
}
if cs.iamToken != "" {
h.ws.Disconnect(clientID, cs.iamToken)
}
h.mu.Lock()
delete(h.clients, clientID)
h.mu.Unlock()
return
}

if tc, ok := conn.UnderlyingConn().(*net.TCPConn); ok {
tc.SetNoDelay(true)
}

h.mu.Lock()
if _, ok := h.clients[clientID]; !ok {
h.mu.Unlock()
conn.Close()
return
}
cs.targetWS = conn
pending := cs.pending
cs.pending = nil
h.mu.Unlock()

// Flush buffered messages
for _, m := range pending {
conn.WriteMessage(m.msgType, m.data)
}

	log.Printf("[INFO] target connected clientID=%s url=%s took=%v pendingFlushed=%d", clientID, targetURL, time.Since(start), len(pending))
h.readFromTarget(clientID, conn, cs)
}

func (h *Handler) onDataC2T(f protocol.Frame) {
msgType := websocket.BinaryMessage
if f.Flags&protocol.FlagTextFrame != 0 {
msgType = websocket.TextMessage
}

h.mu.Lock()
cs, ok := h.clients[f.ClientID]
if !ok {
h.mu.Unlock()
return
}
if cs.targetWS == nil {
// Buffer while connecting
cs.pending = append(cs.pending, pendingMsg{msgType: msgType, data: f.Payload})
h.mu.Unlock()
return
}
h.mu.Unlock()

cs.mu.Lock()
err := cs.targetWS.WriteMessage(msgType, f.Payload)
cs.mu.Unlock()
if err != nil {
	log.Printf("[ERROR] write to target failed clientID=%s err=%v", f.ClientID, err)
}
}

func (h *Handler) onClientDisconnected(f protocol.Frame) {
h.mu.Lock()
cs, ok := h.clients[f.ClientID]
if !ok {
h.mu.Unlock()
return
}
delete(h.clients, f.ClientID)
h.mu.Unlock()

cs.cancel()
if cs.targetWS != nil {
cs.targetWS.Close()
}
	log.Printf("[INFO] client disconnected, closed target clientID=%s", f.ClientID)
}

func (h *Handler) readFromTarget(clientID string, conn *websocket.Conn, cs *clientState) {
defer func() {
conn.Close()
h.mu.Lock()
_, ok := h.clients[clientID]
if ok {
delete(h.clients, clientID)
}
h.mu.Unlock()
if ok {
		log.Printf("[INFO] target disconnected clientID=%s", clientID)
if cs.iamToken != "" {
h.ws.Disconnect(clientID, cs.iamToken)
}
}
}()

for {
msgType, data, err := conn.ReadMessage()
if err != nil {
return
}

dataType := "BINARY"
if msgType == websocket.TextMessage {
dataType = "TEXT"
}
if cs.iamToken == "" {
		log.Printf("[WARN] no IAM token, dropping clientID=%s", clientID)
continue
}

// Per-client mutex ensures ordering
cs.mu.Lock()
sendErr := h.ws.Send(clientID, data, dataType, cs.iamToken)
cs.mu.Unlock()
if sendErr != nil {
		log.Printf("[ERROR] send to client failed clientID=%s err=%v", clientID, sendErr)
}
}
}

func (h *Handler) CloseAll() {
h.mu.Lock()
defer h.mu.Unlock()
for id, cs := range h.clients {
cs.cancel()
if cs.targetWS != nil {
cs.targetWS.Close()
}
delete(h.clients, id)
}
}