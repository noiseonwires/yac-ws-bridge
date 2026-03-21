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

	ups = upstream.New(cfg, func(f protocol.Frame) {
		switch f.Type {
		// --- Control ---
		case protocol.MsgPeerConn:
			peerID, iamToken, err := protocol.DecodePeerConn(f.Payload)
			if err != nil {
				log.Printf("[WARN] bad PEER_CONN: %v", err)
				return
			}
			if ups.IsStaleConnID(peerID) {
				log.Printf("[WARN] PEER_CONN with stale ID %s, ignoring (waiting for fresh ID)", peerID)
				return
			}
			ups.ClearStaleConnID()
			log.Printf("[INFO] PEER_CONN received: peerID=%s tokenLen=%d", peerID, len(iamToken))
			ups.SetPeerConnID(peerID)
			if iamToken != "" {
				ups.SetIAMToken(iamToken)
			}
		case protocol.MsgPeerGone:
			log.Printf("[INFO] PEER_GONE received, closing %d streams", sm.Count())
			ups.SetPeerConnID("")
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
			sm.HandleData(f.StreamID, f.Payload)
		case protocol.MsgFin:
			log.Printf("[INFO] FIN received stream=%d", f.StreamID)
			sm.HandleFin(f.StreamID)
		case protocol.MsgRst:
			log.Printf("[INFO] RST received stream=%d", f.StreamID)
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
	log.Printf("[INFO] helper starting bridge=%s listen=%s relay=%v", cfg.Bridge.URL, cfg.Listen.Address, relay)

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
			go handleConn(ctx, conn, ups, sm, &pendingMu, pendingOpens)
		}
	}()

	// Close listener on shutdown
	go func() { <-ctx.Done(); ln.Close() }()

	// Run upstream (blocks until ctx cancelled)
	ups.Run(ctx)
}

func handleConn(ctx context.Context, conn net.Conn, ups *upstream.Upstream, sm *streams.Manager, pendingMu *sync.Mutex, pendingOpens map[uint32]chan protocol.Frame) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	sid := sm.NextID()
	log.Printf("[INFO] new TCP connection remote=%s stream=%d", conn.RemoteAddr(), sid)

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

	// Wait for OPEN_OK or OPEN_FAIL (with timeout from context)
	select {
	case resp := <-ch:
		if resp.Type == protocol.MsgOpenFail {
			log.Printf("[INFO] stream rejected stream=%d reason=%s", sid, string(resp.Payload))
			conn.Close()
			return
		}
		log.Printf("[INFO] stream opened stream=%d remote=%s", sid, conn.RemoteAddr())
	case <-ctx.Done():
		log.Printf("[INFO] stream cancelled during open stream=%d", sid)
		conn.Close()
		return
	}

	// Stream is open
	sm.Register(s)
	sm.ReadLoop(s)
}
