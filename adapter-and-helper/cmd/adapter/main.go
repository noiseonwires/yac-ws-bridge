package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bridge-to-freedom/adapter/internal/config"
	"github.com/bridge-to-freedom/adapter/internal/protocol"
	"github.com/bridge-to-freedom/adapter/internal/tun"
	"github.com/bridge-to-freedom/adapter/internal/upstream"
	"github.com/bridge-to-freedom/adapter/internal/wsapi"
)

// adapter: opens a TUN on the server, ships every packet from the helper to
// that TUN, and forwards everything the kernel emits back over the bridge.
// IP forwarding / NAT (e.g. iptables MASQUERADE on Linux) is the operator's
// responsibility — see README.
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

	addr, err := cfg.TunAddress()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	peer, err := cfg.TunPeerAddress()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	dev, err := tun.Open(tun.Config{
		Name:          cfg.Tun.Name,
		Address:       addr,
		PeerAddress:   peer,
		MTU:           cfg.Tun.MTU,
		CoalesceDelay: cfg.CoalesceDelay(),
	})
	if err != nil {
		log.Fatalf("open TUN: %v", err)
	}
	defer dev.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsClient := wsapi.NewClient()

	var ups *upstream.Upstream

	installSend := func() {
		dev.SetSend(func(data []byte) error {
			peerID := ups.PeerConnID()
			token := ups.IAMToken()
			if peerID == "" || token == "" {
				return fmt.Errorf("no peer connected")
			}
			if err := wsClient.Send(peerID, data, "BINARY", token); err != nil {
				ups.MarkPeerStale()
				return err
			}
			return nil
		})
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
			log.Printf("[INFO] PEER_CONN received: peerID=%s tokenLen=%d", peerID, len(iamToken))
			ups.SetPeerConnID(peerID)
			if iamToken != "" {
				ups.SetIAMToken(iamToken)
			}
			installSend()

		case protocol.MsgPeerGone:
			rx, tx := dev.Stats()
			log.Printf("[INFO] PEER_GONE received (rxPkts=%d txPkts=%d)", rx, tx)
			ups.SetPeerConnID("")
			dev.SetSend(nil)

		case protocol.MsgPong:
			iamToken, err := protocol.DecodePong(f.Payload)
			if err != nil {
				log.Printf("[WARN] bad PONG: %v", err)
				return
			}
			ups.SetIAMToken(iamToken)

		case protocol.MsgPacket:
			if err := dev.WritePacket(f.Payload); err != nil {
				log.Printf("[WARN] tun write: %v", err)
			}

		case protocol.MsgPacketBatch:
			_, err := protocol.DecodePacketBatch(f.Payload, func(pkt []byte) {
				if werr := dev.WritePacket(pkt); werr != nil {
					log.Printf("[WARN] tun write: %v", werr)
				}
			})
			if err != nil {
				log.Printf("[WARN] bad PACKET_BATCH: %v", err)
			}

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
		dev.Close()
		cancel()
	}()

	// HTTP server for /conn-ids (used by the cloud function on cold start)
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
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
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

	go dev.ReadLoop(ctx)

	log.Printf("[INFO] adapter starting bridge=%s tun=%s addr=%s peer=%s coalesce=%v",
		cfg.Bridge.URL, dev.Name(), addr, peer, cfg.CoalesceDelay())

	ups.Run(ctx)
}
