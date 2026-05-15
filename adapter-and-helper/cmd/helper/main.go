package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bridge-to-freedom/adapter/internal/config"
	"github.com/bridge-to-freedom/adapter/internal/protocol"
	"github.com/bridge-to-freedom/adapter/internal/streams"
	"github.com/bridge-to-freedom/adapter/internal/upstream"
	"github.com/bridge-to-freedom/adapter/internal/wsapi"
)

func main() {
	cfgPath := "helper.config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsClient := wsapi.NewClient()
	relay := cfg.WsAPI.Relay

	var ups *upstream.Upstream

	// pendingOpens tracks streams waiting for OPEN_OK/OPEN_FAIL.
	var pendingMu sync.Mutex
	pendingOpens := make(map[uint32]chan protocol.Frame)

	// probeChans routes incoming DATA/FIN/RST frames whose streamID has the
	// PROBE bit set to the goroutine that initiated the probe (runProbe).
	// Probe streams aren't registered with the streams.Manager; the adapter
	// synthesises the entire HTTP exchange on its side.
	var probeMu sync.Mutex
	probeChans := make(map[uint32]chan protocol.Frame)

	// cancelPendingOpens closes all pending OPEN channels so blocked
	// handleConn goroutines wake up and clean up. Used on PEER_GONE
	// and PEER_CONN to avoid leaked goroutines after adapter restarts.
	cancelPendingOpens := func(reason string) {
		pendingMu.Lock()
		count := len(pendingOpens)
		for sid, ch := range pendingOpens {
			close(ch)
			delete(pendingOpens, sid)
		}
		pendingMu.Unlock()
		if count > 0 {
			log.Printf("[INFO] cancelled %d pending opens: %s", count, reason)
		}
	}

	sm := streams.NewManager(func(data []byte) error {
		if relay {
			// Relay mode: send through upstream WS
			return ups.Send(data)
		}
		// Direct mode: wsSend to adapter
		peerID := ups.PeerConnID()
		token := ups.IAMToken()
		if peerID == "" || token == "" {
			return fmt.Errorf("no peer connected")
		}
		err := wsClient.Send(peerID, data, "BINARY", token)
		if err != nil {
			ups.MarkPeerStale()
		}
		return err
	})
	sm.CoalesceDelay = cfg.CoalesceDelay()

	ups = upstream.New(cfg, func(f protocol.Frame) {
		switch f.Type {
		// --- Control ---
		case protocol.MsgPeerConn:
			peerID, iamToken, _, err := protocol.DecodePeerConn(f.Payload)
			if err != nil {
				log.Printf("[WARN] bad PEER_CONN: %v", err)
				return
			}
			if ups.IsStaleConnID(peerID) {
				log.Printf("[WARN] PEER_CONN with stale ID %s, ignoring (waiting for fresh ID)", peerID)
				return
			}
			ups.ClearStaleConnID()
			cancelPendingOpens("new peer connected")
			log.Printf("[INFO] PEER_CONN received: peerID=%s tokenLen=%d", peerID, len(iamToken))
			ups.SetPeerConnID(peerID)
			if iamToken != "" {
				ups.SetIAMToken(iamToken)
			}
		case protocol.MsgPeerGone:
			log.Printf("[INFO] PEER_GONE received, closing %d streams", sm.Count())
			ups.SetPeerConnID("")
			cancelPendingOpens("peer gone")
			sm.CloseAll()
		case protocol.MsgPong:
			iamToken, err := protocol.DecodePong(f.Payload)
			if err != nil {
				log.Printf("[WARN] bad PONG: %v", err)
				return
			}
			log.Printf("[DEBUG] PONG received, tokenLen=%d", len(iamToken))
			ups.SetIAMToken(iamToken)

		// --- Stream responses ---
		case protocol.MsgOpenOK, protocol.MsgOpenFail:
			typeName := "OPEN_OK"
			if f.Type == protocol.MsgOpenFail {
				typeName = "OPEN_FAIL"
			}
			log.Printf("[INFO] %s received stream=%d", typeName, f.StreamID)
			pendingMu.Lock()
			ch, ok := pendingOpens[f.StreamID]
			pendingMu.Unlock()
			if ok {
				ch <- f
			} else {
				log.Printf("[WARN] %s for unknown stream=%d", typeName, f.StreamID)
			}

		case protocol.MsgData:
			if protocol.IsProbe(f.StreamID) {
				deliverProbeFrame(&probeMu, probeChans, f)
				return
			}
			sm.HandleData(f.StreamID, f.Payload)
		case protocol.MsgFin:
			log.Printf("[INFO] FIN received stream=%d seq=%d", f.StreamID, f.SeqID)
			if protocol.IsProbe(f.StreamID) {
				deliverProbeFrame(&probeMu, probeChans, f)
				return
			}
			sm.HandleFin(f.StreamID)
		case protocol.MsgRst:
			log.Printf("[INFO] RST received stream=%d seq=%d", f.StreamID, f.SeqID)
			if protocol.IsProbe(f.StreamID) {
				deliverProbeFrame(&probeMu, probeChans, f)
				return
			}
			sm.HandleRst(f.StreamID)
		default:
			log.Printf("[WARN] unknown frame type=0x%02x stream=%d", f.Type, f.StreamID)
		}
	})

	// Signal handling
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("[INFO] shutting down")
		go func() {
			time.Sleep(3 * time.Second)
			log.Println("[WARN] graceful shutdown timed out, forcing exit")
			os.Exit(1)
		}()
		sm.CloseAll()
		cancel()
	}()

	// TCP listener
	ln, err := net.Listen("tcp", cfg.Listen.Address)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("[INFO] helper starting bridge=%s listen=%s relay=%v coalesce=%v", cfg.Bridge.URL, cfg.Listen.Address, relay, cfg.CoalesceDelay())

	// Accept loop in background
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[WARN] accept error: %v", err)
				continue
			}
			go handleConn(ctx, conn, ups, sm, &pendingMu, pendingOpens, relay)
		}
	}()

	// Close listener on shutdown
	go func() { <-ctx.Done(); ln.Close() }()

	// Fire a one-shot connectivity probe once the upstream is up. This verifies
	// the wsApi bidirectional data path (helper→cloud→adapter and back) without
	// requiring the user to make any real downstream traffic, and independently
	// of whether the adapter's configured target is reachable.
	go runProbe(ctx, ups, sm, &pendingMu, pendingOpens, &probeMu, probeChans, relay)

	// Run upstream (blocks until ctx cancelled)
	ups.Run(ctx)
}

func handleConn(ctx context.Context, conn net.Conn, ups *upstream.Upstream, sm *streams.Manager, pendingMu *sync.Mutex, pendingOpens map[uint32]chan protocol.Frame, relay bool) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	// Wait for peer readiness before allocating a stream ID. We must wait
	// because the helperShortID (top byte of every stream ID we generate) is
	// assigned by the cloud function and arrives in HELLO_OK; if we allocate
	// before HELLO_OK we'd use shortID=0 and the adapter wouldn't know whom
	// to route the response to.
	if !waitForPeer(ctx, ups, relay, 10*time.Second) {
		log.Printf("[WARN] no peer available, closing inbound TCP from %s", conn.RemoteAddr())
		conn.Close()
		return
	}

	shortID := ups.HelperShortID()
	localID := sm.NextID() & protocol.StreamLocalIDMask // 23-bit local ID (top bit reserved for PROBE)
	sid := (uint32(shortID) << protocol.StreamHelperShortIDShift) | localID
	log.Printf("[INFO] new TCP connection remote=%s stream=%d (shortID=%d local=%d)", conn.RemoteAddr(), sid, shortID, localID)

	s := &streams.Stream{ID: sid, Conn: conn}

	// Register pending open
	ch := make(chan protocol.Frame, 1)
	pendingMu.Lock()
	pendingOpens[sid] = ch
	pendingMu.Unlock()

	defer func() {
		pendingMu.Lock()
		delete(pendingOpens, sid)
		pendingMu.Unlock()
	}()

	// Send OPEN
	if err := sm.SendFrame(protocol.Frame{Type: protocol.MsgOpen, StreamID: sid}); err != nil {
		log.Printf("[WARN] send OPEN failed stream=%d err=%v", sid, err)
		conn.Close()
		return
	}
	log.Printf("[INFO] OPEN sent stream=%d, waiting for response...", sid)

	// Wait for OPEN_OK or OPEN_FAIL (with timeout)
	select {
	case resp, ok := <-ch:
		if !ok {
			// Channel closed — peer disconnected/reconnected while we were waiting
			log.Printf("[INFO] stream aborted during open (peer reset) stream=%d", sid)
			conn.Close()
			return
		}
		if resp.Type == protocol.MsgOpenFail {
			log.Printf("[INFO] stream rejected stream=%d reason=%s", sid, string(resp.Payload))
			conn.Close()
			return
		}
		log.Printf("[INFO] stream opened stream=%d remote=%s", sid, conn.RemoteAddr())
	case <-time.After(30 * time.Second):
		log.Printf("[WARN] OPEN timeout stream=%d (no response in 30s)", sid)
		conn.Close()
		return
	case <-ctx.Done():
		log.Printf("[INFO] stream cancelled during open stream=%d", sid)
		conn.Close()
		return
	}

	// Stream is open
	sm.Register(s)
	sm.ReadLoop(s)
}

// waitForPeer blocks until the peer is available (or relay mode is ready),
// sending a SYNC to speed up discovery. Returns false on timeout or
// cancellation.
//
// Even in relay mode we must wait for HELLO_OK to complete (signalled by a
// non-empty OwnConnID), because the helperShortID assigned to us by the cloud
// function is delivered in HELLO_OK and we stamp it into the top byte of
// every streamID we allocate. Allocating before HELLO_OK would yield
// shortID=0 and the adapter wouldn't be able to route response frames.
func waitForPeer(ctx context.Context, ups *upstream.Upstream, relay bool, timeout time.Duration) bool {
	ready := func() bool {
		if relay {
			return ups.OwnConnID() != ""
		}
		return ups.PeerConnID() != ""
	}
	if ready() {
		return true
	}

	// Send SYNC to ask the cloud function for the current adapter ID (no-op
	// in relay mode where we just need HELLO_OK, but harmless).
	if !relay {
		log.Printf("[DEBUG] peer unknown, sending SYNC for discovery")
		ups.SendSync()
	} else {
		log.Printf("[DEBUG] upstream not yet authenticated (relay), waiting for HELLO_OK")
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	// Poll frequently, re-send SYNC every 2s in case the cloud function
	// instance handling our first SYNC didn't know the adapter yet (serverless
	// state is per-instance, a subsequent SYNC may hit a warmer instance).
	pollTicker := time.NewTicker(200 * time.Millisecond)
	defer pollTicker.Stop()
	syncTicker := time.NewTicker(2 * time.Second)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-syncTicker.C:
			if !relay && ups.PeerConnID() == "" {
				log.Printf("[DEBUG] re-sending SYNC for discovery")
				ups.SendSync()
			}
		case <-pollTicker.C:
			if ready() {
				return true
			}
		}
	}
}

// deliverProbeFrame hands a DATA/FIN/RST frame for a probe-flagged stream to
// the channel registered by runProbe (if any). If no channel is registered
// (probe finished or cancelled), the frame is silently dropped.
func deliverProbeFrame(mu *sync.Mutex, probeChans map[uint32]chan protocol.Frame, f protocol.Frame) {
	mu.Lock()
	ch, ok := probeChans[f.StreamID]
	mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- f:
	default:
		log.Printf("[WARN] probe channel full, dropping frame stream=%d type=0x%02x", f.StreamID, f.Type)
	}
}

// Probe retry policy. Mirrors the MAUI client's RunProbeAsync.
const (
	probeMaxAttempts = 3
	probeRetryDelay  = 3 * time.Second
)

// runProbe drives the connectivity probe with retries. It logs a single
// summary line per attempt and a final OK/FAILED line so log readers can see
// the verification status at a glance.
//
// Designed to be fire-and-forget. Failures are logged but do not affect the
// helper's normal operation.
func runProbe(
	ctx context.Context,
	ups *upstream.Upstream,
	sm *streams.Manager,
	pendingMu *sync.Mutex, pendingOpens map[uint32]chan protocol.Frame,
	probeMu *sync.Mutex, probeChans map[uint32]chan protocol.Frame,
	relay bool,
) {
	// Wait for the upstream to be ready (HELLO_OK + peer known in non-relay).
	if !waitForPeer(ctx, ups, relay, 30*time.Second) {
		log.Printf("[WARN] probe: upstream not ready within 30s, skipping connectivity probe")
		return
	}

	var lastErr string
	for attempt := 1; attempt <= probeMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if attempt > 1 {
			log.Printf("[INFO] probe: retrying in %v (%d/%d) — last: %s",
				probeRetryDelay, attempt, probeMaxAttempts, lastErr)
			select {
			case <-time.After(probeRetryDelay):
			case <-ctx.Done():
				return
			}
		}
		ok, detail := tryProbeOnce(ctx, ups, sm, pendingMu, pendingOpens, probeMu, probeChans, attempt)
		if ctx.Err() != nil {
			return
		}
		if ok {
			log.Printf("[INFO] probe: OK after %d attempt(s) — %s (wsApi bidirectional data path verified)",
				attempt, detail)
			return
		}
		lastErr = detail
	}
	log.Printf("[WARN] probe: FAILED after %d attempts: %s", probeMaxAttempts, lastErr)
}

// tryProbeOnce performs a single probe round-trip. Returns (true, "rtt=…")
// on success, or (false, "<reason>") on any failure mode (timeout, OPEN_FAIL,
// RST, unexpected body, send error). Does not retry; runProbe does that.
func tryProbeOnce(
	ctx context.Context,
	ups *upstream.Upstream,
	sm *streams.Manager,
	pendingMu *sync.Mutex, pendingOpens map[uint32]chan protocol.Frame,
	probeMu *sync.Mutex, probeChans map[uint32]chan protocol.Frame,
	attempt int,
) (bool, string) {
	shortID := ups.HelperShortID()
	localID := sm.NextID() & protocol.StreamLocalIDMask
	sid := (uint32(shortID) << protocol.StreamHelperShortIDShift) | protocol.StreamProbeFlag | localID

	openCh := make(chan protocol.Frame, 1)
	pendingMu.Lock()
	pendingOpens[sid] = openCh
	pendingMu.Unlock()
	defer func() {
		pendingMu.Lock()
		delete(pendingOpens, sid)
		pendingMu.Unlock()
	}()

	dataCh := make(chan protocol.Frame, 16)
	probeMu.Lock()
	probeChans[sid] = dataCh
	probeMu.Unlock()
	defer func() {
		probeMu.Lock()
		delete(probeChans, sid)
		probeMu.Unlock()
	}()

	start := time.Now()
	log.Printf("[INFO] probe attempt %d: sending OPEN stream=%d (shortID=%d, PROBE=1)", attempt, sid, shortID)
	if err := sm.SendFrame(protocol.Frame{Type: protocol.MsgOpen, StreamID: sid}); err != nil {
		log.Printf("[WARN] probe attempt %d: OPEN send failed: %v", attempt, err)
		return false, "send failed: " + err.Error()
	}

	// Wait for OPEN_OK.
	select {
	case resp, ok := <-openCh:
		if !ok {
			log.Printf("[WARN] probe attempt %d: peer reset during OPEN stream=%d", attempt, sid)
			return false, "peer reset"
		}
		if resp.Type == protocol.MsgOpenFail {
			reason := string(resp.Payload)
			log.Printf("[WARN] probe attempt %d: OPEN_FAIL stream=%d reason=%s", attempt, sid, reason)
			return false, "OPEN_FAIL: " + reason
		}
		log.Printf("[INFO] probe attempt %d: OPEN_OK stream=%d rtt=%v", attempt, sid, time.Since(start))
	case <-time.After(10 * time.Second):
		log.Printf("[WARN] probe attempt %d: OPEN_OK timeout stream=%d (10s)", attempt, sid)
		return false, "OPEN_OK timeout"
	case <-ctx.Done():
		return false, "cancelled"
	}

	// Send a token GET to make this look like a real HTTP request on the wire.
	getReq := "GET / HTTP/1.0\r\nHost: probe.bridge-to-freedom\r\nUser-Agent: btf-helper-probe\r\n\r\n"
	if err := sm.SendFrame(protocol.Frame{
		Type:     protocol.MsgData,
		StreamID: sid,
		Payload:  []byte(getReq),
	}); err != nil {
		log.Printf("[WARN] probe attempt %d: GET send failed: %v", attempt, err)
		return false, "GET send failed: " + err.Error()
	}

	// Read response until FIN or timeout.
	var body []byte
	deadline := time.After(10 * time.Second)
	finSeen := false
loop:
	for !finSeen {
		select {
		case f := <-dataCh:
			switch f.Type {
			case protocol.MsgData:
				body = append(body, f.Payload...)
			case protocol.MsgFin:
				finSeen = true
				break loop
			case protocol.MsgRst:
				log.Printf("[WARN] probe attempt %d: RST stream=%d after %d bytes", attempt, sid, len(body))
				return false, "RST received"
			}
		case <-deadline:
			log.Printf("[WARN] probe attempt %d: response timeout stream=%d after %d bytes (no FIN in 10s)",
				attempt, sid, len(body))
			return false, "response timeout"
		case <-ctx.Done():
			return false, "cancelled"
		}
	}

	rtt := time.Since(start)
	if len(body) == 0 {
		log.Printf("[WARN] probe attempt %d: empty response stream=%d rtt=%v", attempt, sid, rtt)
		return false, "empty response"
	}
	const expected = "HTTP/1.1 200 OK"
	if len(body) >= len(expected) && string(body[:len(expected)]) == expected {
		log.Printf("[INFO] probe attempt %d: OK stream=%d rtt=%v bytes=%d", attempt, sid, rtt, len(body))
		return true, fmt.Sprintf("rtt=%v bytes=%d", rtt, len(body))
	}
	preview := string(body)
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	log.Printf("[WARN] probe attempt %d: unexpected response stream=%d rtt=%v bytes=%d preview=%q",
		attempt, sid, rtt, len(body), preview)
	return false, "unexpected response"
}
