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
	"github.com/bridge-to-freedom/adapter/internal/quictun"
	"github.com/bridge-to-freedom/adapter/internal/streams"
	"github.com/bridge-to-freedom/adapter/internal/upstream"
	"github.com/bridge-to-freedom/adapter/internal/wsapi"
	"github.com/quic-go/quic-go"
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

	// Virtual transport: outbound QUIC packets → wsSend or relay.
	// Async send workers decouple QUIC pacing from wsSend latency.
	transport := quictun.NewTransport(func(data []byte) error {
		frame := protocol.Encode(protocol.Frame{
			Type:    protocol.MsgQUIC,
			Payload: data,
		})
		if relay {
			return ups.Send(frame)
		}
		peerID := ups.PeerConnID()
		token := ups.IAMToken()
		if peerID == "" || token == "" {
			return fmt.Errorf("no peer connected")
		}
		err := wsClient.Send(peerID, frame, "BINARY", token)
		if err != nil {
			ups.MarkPeerStale()
		}
		return err
	}, cfg.SendWorkers())

	// QUIC connection to the adapter (managed with reconnection).
	var quicMu sync.Mutex
	var quicConn quic.Connection

	getOrDialQUIC := func(ctx context.Context) (quic.Connection, error) {
		quicMu.Lock()
		defer quicMu.Unlock()
		if quicConn != nil {
			// Check if connection is still alive.
			select {
			case <-quicConn.Context().Done():
				log.Printf("[INFO] cached QUIC connection is dead, re-dialing")
				quicConn = nil
			default:
			}
		}
		if quicConn != nil {
			return quicConn, nil
		}
		log.Printf("[INFO] dialing QUIC to adapter (virtual transport)")
		qc, err := quictun.DialQUIC(ctx, transport, cfg.MTU())
		if err != nil {
			return nil, fmt.Errorf("QUIC dial: %w", err)
		}
		quicConn = qc
		log.Printf("[INFO] QUIC connection established")
		return qc, nil
	}

	invalidateQUIC := func(dead quic.Connection) {
		quicMu.Lock()
		defer quicMu.Unlock()
		if quicConn == dead {
			log.Printf("[INFO] invalidating dead QUIC connection")
			quicConn = nil
		}
	}

	closeQUIC := func() {
		quicMu.Lock()
		defer quicMu.Unlock()
		if quicConn != nil {
			quicConn.CloseWithError(0, "peer gone")
			quicConn = nil
		}
	}

	ups = upstream.New(cfg, func(f protocol.Frame) {
		switch f.Type {
		case protocol.MsgPeerConn:
			peerID, iamToken, err := protocol.DecodePeerConn(f.Payload)
			if err != nil {
				log.Printf("[WARN] bad PEER_CONN: %v", err)
				return
			}
			if ups.IsStaleConnID(peerID) {
				log.Printf("[WARN] PEER_CONN with stale ID %s, ignoring", peerID)
				return
			}
			ups.ClearStaleConnID()
			// New adapter connected — close old QUIC connection to force fresh handshake.
			closeQUIC()
			log.Printf("[INFO] PEER_CONN received: peerID=%s tokenLen=%d", peerID, len(iamToken))
			ups.SetPeerConnID(peerID)
			if iamToken != "" {
				ups.SetIAMToken(iamToken)
			}
		case protocol.MsgPeerGone:
			log.Printf("[INFO] PEER_GONE received")
			ups.SetPeerConnID("")
			closeQUIC()
		case protocol.MsgPong:
			iamToken, err := protocol.DecodePong(f.Payload)
			if err != nil {
				log.Printf("[WARN] bad PONG: %v", err)
				return
			}
			log.Printf("[DEBUG] PONG received, tokenLen=%d", len(iamToken))
			ups.SetIAMToken(iamToken)
		case protocol.MsgQUIC:
			transport.Deliver(f.Payload)
		default:
			log.Printf("[WARN] unknown frame type=0x%02x", f.Type)
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
		closeQUIC()
		transport.Close()
		cancel()
	}()

	// TCP listener — accept local connections and tunnel over QUIC.
	ln, err := net.Listen("tcp", cfg.Listen.Address)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("[INFO] helper starting bridge=%s listen=%s relay=%v mtu=%d sendWorkers=%d", cfg.Bridge.URL, cfg.Listen.Address, relay, cfg.MTU(), cfg.SendWorkers())

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
			go handleConn(ctx, conn, ups, relay, getOrDialQUIC, invalidateQUIC)
		}
	}()

	go func() { <-ctx.Done(); ln.Close() }()

	ups.Run(ctx)
}

func handleConn(ctx context.Context, conn net.Conn, ups *upstream.Upstream, relay bool, dialFn func(context.Context) (quic.Connection, error), invalidateFn func(quic.Connection)) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	log.Printf("[INFO] new TCP connection remote=%s", conn.RemoteAddr())

	// Wait for peer to be available.
	if !waitForPeer(ctx, ups, relay, 10*time.Second) {
		log.Printf("[WARN] no peer available, closing connection")
		conn.Close()
		return
	}

	// Try to open a QUIC stream; on failure invalidate the connection and retry once.
	var qconn quic.Connection
	var qs quic.Stream
	for attempt := 0; attempt < 2; attempt++ {
		var err error
		qconn, err = dialFn(ctx)
		if err != nil {
			log.Printf("[WARN] QUIC dial failed: %v", err)
			conn.Close()
			return
		}
		qs, err = qconn.OpenStreamSync(ctx)
		if err == nil {
			break
		}
		log.Printf("[WARN] QUIC open stream failed (attempt %d): %v", attempt+1, err)
		invalidateFn(qconn)
		if attempt == 1 {
			conn.Close()
			return
		}
	}

	log.Printf("[INFO] QUIC stream opened id=%d remote=%s", qs.StreamID(), conn.RemoteAddr())
	streams.Relay(qs, conn)
	log.Printf("[INFO] stream finished id=%d", qs.StreamID())
}

func waitForPeer(ctx context.Context, ups *upstream.Upstream, relay bool, timeout time.Duration) bool {
	if relay {
		return true
	}
	if ups.PeerConnID() != "" {
		return true
	}

	log.Printf("[DEBUG] peer unknown, sending SYNC for discovery")
	ups.SendSync()

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
			if ups.PeerConnID() == "" {
				log.Printf("[DEBUG] re-sending SYNC for discovery")
				ups.SendSync()
			}
		case <-pollTicker.C:
			if ups.PeerConnID() != "" {
				return true
			}
		}
	}
}

