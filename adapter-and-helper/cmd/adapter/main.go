package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	cfgPath := "adapter.config.yaml"
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

	var ups *upstream.Upstream

	// Virtual transport: outbound QUIC packets → wsSend (gRPC) to helper.
	// Async send workers decouple QUIC pacing from wsSend latency.
	transport := quictun.NewTransport(func(data []byte) error {
		peerID := ups.PeerConnID()
		token := ups.IAMToken()
		if peerID == "" || token == "" {
			return fmt.Errorf("no peer connected")
		}
		frame := protocol.Encode(protocol.Frame{
			Type:    protocol.MsgQUIC,
			Payload: data,
		})
		err := wsClient.Send(peerID, frame, "BINARY", token)
		if err != nil {
			ups.MarkPeerStale()
		}
		return err
	}, cfg.SendWorkers())

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
			log.Printf("[INFO] PEER_CONN received: peerID=%s tokenLen=%d", peerID, len(iamToken))
			ups.SetPeerConnID(peerID)
			if iamToken != "" {
				ups.SetIAMToken(iamToken)
			}
		case protocol.MsgPeerGone:
			log.Printf("[INFO] PEER_GONE received")
			ups.SetPeerConnID("")
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
		transport.Close()
		cancel()
	}()

	// HTTP server for /conn-ids
	if cfg.HTTP.ListenPort > 0 {
		mux := http.NewServeMux()
		mux.HandleFunc("/conn-ids", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token != cfg.Bridge.AuthToken {
				log.Printf("[WARN] /conn-ids unauthorized request from %s", r.RemoteAddr)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			own := ups.OwnConnID()
			peer := ups.PeerConnID()
			if own == "" {
				log.Printf("[INFO] /conn-ids requested but adapter not connected yet")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			log.Printf("[INFO] /conn-ids requested adapterConnId=%s helperConnId=%s", own, peer)
			resp := map[string]string{
				"adapterConnId": own,
				"helperConnId":  peer,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		})
		addr := fmt.Sprintf(":%d", cfg.HTTP.ListenPort)
		srv := &http.Server{Addr: addr, Handler: mux}
		go func() {
			log.Printf("[INFO] HTTP server starting addr=%s", addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server: %v", err)
			}
		}()
		go func() { <-ctx.Done(); srv.Close() }()
	}

	// Start QUIC server (adapter side accepts QUIC connections from helper).
	go runQUICServer(ctx, cfg, transport)

	log.Printf("[INFO] adapter starting bridge=%s target=%s mtu=%d sendWorkers=%d", cfg.Bridge.URL, cfg.Target.Address, cfg.MTU(), cfg.SendWorkers())
	ups.Run(ctx)
}

func runQUICServer(ctx context.Context, cfg *config.Config, transport *quictun.Transport) {
	ln, err := quictun.ListenQUIC(ctx, transport, cfg.MTU())
	if err != nil {
		log.Fatalf("QUIC listen: %v", err)
	}
	defer ln.Close()
	log.Printf("[INFO] QUIC server listening (virtual transport)")

	for {
		qconn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[WARN] QUIC accept: %v", err)
			continue
		}
		log.Printf("[INFO] QUIC connection accepted")
		go acceptStreams(ctx, cfg, qconn)
	}
}

func acceptStreams(ctx context.Context, cfg *config.Config, qconn quic.Connection) {
	defer qconn.CloseWithError(0, "done")
	for {
		qs, err := qconn.AcceptStream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[INFO] QUIC connection closed: %v", err)
			return
		}
		go handleStream(cfg, qs)
	}
}

func handleStream(cfg *config.Config, qs quic.Stream) {
	log.Printf("[INFO] QUIC stream accepted id=%d, dialing target=%s", qs.StreamID(), cfg.Target.Address)
	tc, err := net.DialTimeout("tcp", cfg.Target.Address, 10*time.Second)
	if err != nil {
		log.Printf("[WARN] target connect failed stream=%d err=%v", qs.StreamID(), err)
		qs.CancelRead(1)
		qs.Close()
		return
	}
	log.Printf("[INFO] stream relaying id=%d target=%s", qs.StreamID(), cfg.Target.Address)
	streams.Relay(qs, tc)
	log.Printf("[INFO] stream finished id=%d", qs.StreamID())
}


