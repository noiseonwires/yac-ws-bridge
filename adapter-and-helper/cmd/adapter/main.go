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
	"github.com/bridge-to-freedom/adapter/internal/streams"
	"github.com/bridge-to-freedom/adapter/internal/upstream"
	"github.com/bridge-to-freedom/adapter/internal/wsapi"
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

	sm := streams.NewManager(func(data []byte) error {
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
	sm.Reorder = true

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

		// --- Stream ---
		case protocol.MsgOpen, protocol.MsgData, protocol.MsgFin, protocol.MsgRst:
			if f.Type != protocol.MsgData {
				log.Printf("[INFO] %s received stream=%d seq=%d",
					map[byte]string{protocol.MsgOpen: "OPEN", protocol.MsgFin: "FIN", protocol.MsgRst: "RST"}[f.Type],
					f.StreamID, f.SeqID)
			}
			sm.HandleStreamFrame(f, func(of protocol.Frame) {
				switch of.Type {
				case protocol.MsgOpen:
					go handleOpen(cfg, sm, of.StreamID)
				case protocol.MsgData:
					sm.HandleData(of.StreamID, of.Payload)
				case protocol.MsgFin:
					sm.HandleFin(of.StreamID)
				case protocol.MsgRst:
					sm.HandleRst(of.StreamID)
				}
			})
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
		// Hard exit deadline — if graceful shutdown takes too long, force exit
		go func() {
			time.Sleep(3 * time.Second)
			log.Println("[WARN] graceful shutdown timed out, forcing exit")
			os.Exit(1)
		}()
		sm.CloseAll()
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
				log.Printf("[INFO] /conn-ids requested from %s but adapter not connected yet", r.RemoteAddr)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			log.Printf("[INFO] /conn-ids requested from %s adapterConnId=%s helperConnId=%s", r.RemoteAddr, own, peer)
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

	log.Printf("[INFO] adapter starting bridge=%s target=%s coalesce=%v", cfg.Bridge.URL, cfg.Target.Address, cfg.CoalesceDelay())
	ups.Run(ctx)
}

func handleOpen(cfg *config.Config, sm *streams.Manager, streamID uint32) {
	conn, err := net.DialTimeout("tcp", cfg.Target.Address, 10*time.Second)
	if err != nil {
		log.Printf("[WARN] target connect failed stream=%d err=%v", streamID, err)
		sm.SendFrame(protocol.Frame{Type: protocol.MsgOpenFail, StreamID: streamID, Payload: []byte(err.Error())})
		return
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	s := &streams.Stream{ID: streamID, Conn: conn}
	sm.Register(s)

	if err := sm.SendFrame(protocol.Frame{Type: protocol.MsgOpenOK, StreamID: streamID}); err != nil {
		log.Printf("[WARN] send OPEN_OK failed stream=%d err=%v", streamID, err)
		conn.Close()
		sm.Remove(streamID)
		return
	}

	log.Printf("[INFO] stream opened stream=%d target=%s", streamID, cfg.Target.Address)
	sm.ReadLoop(s)
}
