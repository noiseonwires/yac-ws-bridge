package handler

import (
	"context"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bridge-to-freedom/adapter/internal/config"
	"github.com/bridge-to-freedom/adapter/internal/protocol"
	"github.com/bridge-to-freedom/adapter/internal/wsapi"
	"github.com/gorilla/websocket"
)

type clientState struct {
	targetWS         *websocket.Conn
	cancel           context.CancelFunc
	mu               sync.Mutex
	iamToken         string
	pending          []pendingMsg
	pendingFirstAt   time.Time
	reorderTimer     *time.Timer
	lastFlushedSeqID string
}

type pendingMsg struct {
	seqID   string
	arrival uint64
	msgType int
	data    []byte
}

// earlyBuffer holds frames that arrived before CLIENT_CONNECTED
// (serverless CF instances may deliver DATA/DISCONNECT before CONNECT).
type earlyBuffer struct {
	frames       []pendingMsg
	disconnected bool
	createdAt    time.Time
}

type Handler struct {
	mu          sync.Mutex
	clients     map[string]*clientState
	earlyData   map[string]*earlyBuffer
	closedIDs   map[string]time.Time // recently closed clients — drop their late-arriving data
	nextArrival uint64
	cfg         *config.Config
	ws          wsapi.Client
	reorder     reorderConfig

	// lateSeqCloses counts clients reset because a DATA_C2T frame arrived after a
	// newer frame was already flushed (reorder window exceeded). A non-zero and
	// growing value means c2tMaxDelayMs is too small for real-world CF skew OR a
	// frame was lost upstream — either way it explains user-visible resets.
	lateSeqCloses uint64
}

type reorderConfig struct {
	c2tDelay      time.Duration
	c2tMaxDelay   time.Duration
	seqDescending bool
}

func New(cfg *config.Config) *Handler {
	h := &Handler{
		clients:   make(map[string]*clientState),
		earlyData: make(map[string]*earlyBuffer),
		closedIDs: make(map[string]time.Time),
		cfg:       cfg,
		ws:        wsapi.NewClient(cfg.WsApi.Mode),
		reorder: reorderConfig{
			c2tDelay:      cfg.C2TReorderDelay(),
			c2tMaxDelay:   cfg.C2TReorderMaxDelay(),
			seqDescending: cfg.C2TSeqDescending(),
		},
	}
	go h.cleanupEarlyData()
	return h
}

// cleanupEarlyData periodically evicts stale early buffers
// that were never claimed by a CLIENT_CONNECTED.
func (h *Handler) cleanupEarlyData() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		for id, eb := range h.earlyData {
			if time.Since(eb.createdAt) > 30*time.Second {
				log.Printf("[WARN] dropping stale early buffer clientID=%s msgs=%d age=%v", id, len(eb.frames), time.Since(eb.createdAt))
				delete(h.earlyData, id)
			}
		}
		for id, t := range h.closedIDs {
			if time.Since(t) > 30*time.Second {
				delete(h.closedIDs, id)
			}
		}
		h.mu.Unlock()

		if n := atomic.LoadUint64(&h.lateSeqCloses); n > 0 {
			log.Printf("[INFO] reorder stats: lateSeqCloses=%d (clients reset due to out-of-window/lost C2T frames)", n)
		}
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
	// Check for messages/disconnect that arrived before this CLIENT_CONNECTED
	if eb, ok := h.earlyData[f.ClientID]; ok {
		delete(h.earlyData, f.ClientID)
		if eb.disconnected {
			h.mu.Unlock()
			cancel()
			log.Printf("[INFO] client already disconnected before registration clientID=%s earlyMsgs=%d", f.ClientID, len(eb.frames))
			return
		}
		if len(eb.frames) > 0 {
			sort.SliceStable(eb.frames, func(i, j int) bool { return h.reorder.pendingLess(eb.frames[i], eb.frames[j]) })
			cs.pending = append(cs.pending, eb.frames...)
			log.Printf("[INFO] incorporated %d early messages (sorted by alphabetic seqID) for clientID=%s", len(eb.frames), f.ClientID)
		}
	}
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
		h.closedIDs[clientID] = time.Now()
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
	pendingCount := len(cs.pending)
	h.scheduleFlushLocked(clientID, cs)
	h.mu.Unlock()

	log.Printf("[INFO] target connected clientID=%s url=%s took=%v pendingQueued=%d", clientID, targetURL, time.Since(start), pendingCount)
	h.readFromTarget(clientID, conn, cs)
}

func (h *Handler) onDataC2T(f protocol.Frame) {
	msgType := websocket.BinaryMessage
	if f.Flags&protocol.FlagTextFrame != 0 {
		msgType = websocket.TextMessage
	}

	msg := pendingMsg{seqID: f.SeqID, msgType: msgType, data: f.Payload}

	h.mu.Lock()
	msg.arrival = h.nextArrivalLocked()
	cs, ok := h.clients[f.ClientID]
	if !ok {
		// Check if this client was already closed — drop late-arriving data
		if _, closed := h.closedIDs[f.ClientID]; closed {
			h.mu.Unlock()
			return
		}
		// Client not registered yet — buffer (may arrive before CLIENT_CONNECTED in serverless)
		eb, exists := h.earlyData[f.ClientID]
		if !exists {
			eb = &earlyBuffer{createdAt: time.Now()}
			h.earlyData[f.ClientID] = eb
		}
		eb.frames = append(eb.frames, msg)
		h.mu.Unlock()
		log.Printf("[INFO] buffered early DATA_C2T for unregistered client clientID=%s seqID=%s buffered=%d", f.ClientID, f.SeqID, len(eb.frames))
		return
	}
	if h.reorder.isLateSeq(cs.lastFlushedSeqID, f.SeqID) {
		h.mu.Unlock()
		atomic.AddUint64(&h.lateSeqCloses, 1)
		log.Printf("[WARN] late DATA_C2T after newer data was flushed, closing client clientID=%s seqID=%s last=%s", f.ClientID, f.SeqID, cs.lastFlushedSeqID)
		h.closeClient(f.ClientID, cs, "late C2T data")
		return
	}
	cs.pending = append(cs.pending, msg)
	if cs.targetWS == nil {
		// Buffer while connecting; connectToTarget schedules the ordered flush.
		h.mu.Unlock()
		return
	}
	h.scheduleFlushLocked(f.ClientID, cs)
	h.mu.Unlock()
}

func (h *Handler) onClientDisconnected(f protocol.Frame) {
	h.mu.Lock()
	cs, ok := h.clients[f.ClientID]
	if !ok {
		// Check if already closed
		if _, closed := h.closedIDs[f.ClientID]; closed {
			h.mu.Unlock()
			return
		}
		// May arrive before CLIENT_CONNECTED — mark in early buffer
		eb, exists := h.earlyData[f.ClientID]
		if !exists {
			eb = &earlyBuffer{createdAt: time.Now()}
			h.earlyData[f.ClientID] = eb
		}
		eb.disconnected = true
		h.mu.Unlock()
		log.Printf("[INFO] buffered early DISCONNECT for unregistered client clientID=%s", f.ClientID)
		return
	}
	delete(h.clients, f.ClientID)
	h.closedIDs[f.ClientID] = time.Now()
	if cs.reorderTimer != nil {
		cs.reorderTimer.Stop()
		cs.reorderTimer = nil
	}
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
			h.closedIDs[clientID] = time.Now()
			if cs.reorderTimer != nil {
				cs.reorderTimer.Stop()
				cs.reorderTimer = nil
			}
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

func (h *Handler) scheduleFlushLocked(clientID string, cs *clientState) {
	if cs.targetWS == nil || len(cs.pending) == 0 {
		return
	}
	now := time.Now()
	if cs.pendingFirstAt.IsZero() {
		cs.pendingFirstAt = now
	}
	delay := h.reorder.c2tDelay
	if elapsed := now.Sub(cs.pendingFirstAt); elapsed >= h.reorder.c2tMaxDelay {
		delay = 0
	} else if remaining := h.reorder.c2tMaxDelay - elapsed; delay > remaining {
		delay = remaining
	}
	if cs.reorderTimer == nil {
		cs.reorderTimer = time.AfterFunc(delay, func() { h.flushPending(clientID) })
		return
	}
	cs.reorderTimer.Reset(delay)
}

func (h *Handler) flushPending(clientID string) {
	h.mu.Lock()
	cs, ok := h.clients[clientID]
	if !ok || cs.targetWS == nil || len(cs.pending) == 0 {
		h.mu.Unlock()
		return
	}
	pending := append([]pendingMsg(nil), cs.pending...)
	sort.SliceStable(pending, func(i, j int) bool { return h.reorder.pendingLess(pending[i], pending[j]) })
	cs.pending = nil
	cs.pendingFirstAt = time.Time{}
	cs.reorderTimer = nil
	conn := cs.targetWS
	for _, msg := range pending {
		if msg.seqID == "" {
			continue
		}
		// lastFlushedSeqID must track the chronologically NEWEST frame we have
		// delivered, regardless of the configured sort direction.
		if h.reorder.chronoNewer(msg.seqID, cs.lastFlushedSeqID) {
			cs.lastFlushedSeqID = msg.seqID
		}
	}
	h.mu.Unlock()

	for _, msg := range pending {
		cs.mu.Lock()
		err := conn.WriteMessage(msg.msgType, msg.data)
		cs.mu.Unlock()
		if err != nil {
			log.Printf("[ERROR] write to target failed clientID=%s err=%v", clientID, err)
			h.closeClient(clientID, cs, "target write failed")
			return
		}
	}
}

func (h *Handler) closeClient(clientID string, cs *clientState, reason string) {
	h.mu.Lock()
	current, ok := h.clients[clientID]
	if !ok || current != cs {
		h.mu.Unlock()
		return
	}
	delete(h.clients, clientID)
	h.closedIDs[clientID] = time.Now()
	if cs.reorderTimer != nil {
		cs.reorderTimer.Stop()
		cs.reorderTimer = nil
	}
	h.mu.Unlock()

	cs.cancel()
	if cs.targetWS != nil {
		cs.targetWS.Close()
	}
	if cs.iamToken != "" {
		h.ws.Disconnect(clientID, cs.iamToken)
	}
	log.Printf("[WARN] client closed clientID=%s reason=%s", clientID, reason)
}

func (h *Handler) nextArrivalLocked() uint64 {
	h.nextArrival++
	return h.nextArrival
}

// IMPORTANT — sort direction is configurable (reorder.seqOrder), defaulting to
// descending because Yandex Cloud message IDs are REVERSE-chronological
// lexicographically: a LARGER string means an EARLIER message; the newest
// message has the SMALLEST ID. (Verified empirically: the IDs decrease as
// wall-clock time advances, over both seconds and months — a reverse-timestamp
// key scheme.) chronoCmp below normalises this so the rest of the code can think
// purely in chronological terms regardless of the configured direction.

// chronoCmp compares two sequence IDs chronologically. It returns a negative
// number if a is OLDER than b, a positive number if a is NEWER, and zero if they
// are equal. ok is false if either ID is empty.
//
// In descending mode (Yandex default) a larger lexicographic ID is older, so the
// raw lexicographic comparison is inverted. In ascending mode the raw comparison
// is already chronological.
func (r reorderConfig) chronoCmp(a, b string) (int, bool) {
	cmp, ok := compareSeq(a, b)
	if !ok {
		return 0, false
	}
	if r.seqDescending {
		return -cmp, true
	}
	return cmp, true
}

// pendingLess orders buffered messages chronologically (oldest first) for
// in-order delivery to the target. Ties or missing seqIDs fall back to arrival
// order so delivery stays deterministic.
func (r reorderConfig) pendingLess(a, b pendingMsg) bool {
	if cmp, ok := r.chronoCmp(a.seqID, b.seqID); ok && cmp != 0 {
		return cmp < 0
	}
	return a.arrival < b.arrival
}

// isLateSeq reports whether an incoming frame is older than (or the same as) the
// newest frame already flushed, so it can no longer be delivered in order.
// lastSeqID tracks the newest flushed ID (see chronoNewer).
func (r reorderConfig) isLateSeq(lastSeqID, seqID string) bool {
	if cmp, ok := r.chronoCmp(seqID, lastSeqID); ok {
		return cmp <= 0
	}
	return false
}

// chronoNewer reports whether candidate is chronologically newer than current.
// Used to track the newest flushed sequence ID independent of sort direction.
func (r reorderConfig) chronoNewer(candidate, current string) bool {
	if current == "" {
		return true
	}
	cmp, ok := r.chronoCmp(candidate, current)
	return ok && cmp > 0
}

func compareSeq(a, b string) (int, bool) {
	if a == "" || b == "" {
		return 0, false
	}
	switch {
	case a < b:
		return -1, true
	case a > b:
		return 1, true
	default:
		return 0, true
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
		if cs.reorderTimer != nil {
			cs.reorderTimer.Stop()
			cs.reorderTimer = nil
		}
		delete(h.clients, id)
		h.closedIDs[id] = time.Now()
	}
	for id := range h.earlyData {
		delete(h.earlyData, id)
	}
}
