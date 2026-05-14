package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bridge-to-freedom/adapter/internal/config"
	"github.com/bridge-to-freedom/adapter/internal/protocol"
	"github.com/bridge-to-freedom/adapter/internal/tun"
	"github.com/bridge-to-freedom/adapter/internal/upstream"
	"github.com/bridge-to-freedom/adapter/internal/wsapi"
)

// helper: opens a TUN on the client device, ships every packet to the adapter
// over the YC bridge, and writes back whatever the adapter sends.
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
	log.Printf("[INFO] helper build=v5-tun-hdr-fix-2")

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
	relay := cfg.WsAPI.Relay

	var ups *upstream.Upstream

	// installSend wires the TUN's outbound path through either relay-via-WS
	// or direct-via-gRPC, depending on configuration.
	installSend := func() {
		dev.SetSend(func(data []byte) error {
			if relay {
				return ups.Send(data)
			}
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
			// Keep TUN open; just stop being able to send.
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

	// Wire the send path early in relay mode so packets can flow as soon as
	// the upstream WS is up — even before PEER_CONN arrives.
	if relay {
		installSend()
	}

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
		dev.Close() // unblocks ReadLoop
		cancel()
	}()

	// TUN read loop in background; main goroutine runs the upstream.
	go dev.ReadLoop(ctx)

	log.Printf("[INFO] helper starting bridge=%s tun=%s addr=%s peer=%s relay=%v coalesce=%v",
		cfg.Bridge.URL, dev.Name(), addr, peer, relay, cfg.CoalesceDelay())

	ups.Run(ctx)
}
