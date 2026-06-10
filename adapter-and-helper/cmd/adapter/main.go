package main

import (
	"context"
	"crypto/subtle"
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

	// The HTTP recovery endpoint is required: the cloud function polls it on
	// cold start to recover adapter/helper connection IDs. Without it the
	// tunnel will appear to work briefly, but break as soon as the function
	// instance recycles.
	if cfg.HTTP.ListenPort <= 0 {
		log.Fatalf("http.listenPort must be > 0 (the cloud function needs the recovery endpoint to be reachable; set e.g. 8080 in adapter.config.yaml)")
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsClient := wsapi.NewClient()

	var ups *upstream.Upstream
	var sm *streams.Manager

	sm = streams.NewManager(func(data []byte) error {
		// Frame on the wire is [1B type][4B streamID BE][4B seqID BE][payload].
		// The top byte of streamID is the helper short ID assigned by the cloud
		// function; we use it to route per-stream frames back to the originating
		// helper without having to decode the whole frame.
		var shortID byte
		if len(data) >= 2 {
			shortID = data[1]
		}
		peerID := ups.Helper(shortID)
		token := ups.IAMToken()
		if peerID == "" || token == "" {
			return fmt.Errorf("no helper for shortID=%d", shortID)
		}
		err := wsClient.Send(peerID, data, "BINARY", token)
		if err != nil {
			// Drop just this helper; other helpers keep working.
			ups.MarkHelperStale(shortID)
			// Close that helper's streams so we don't keep buffering forever.
			n := sm.CloseHelper(shortID)
			log.Printf("[WARN] helper shortID=%d wsSend failed, closed %d streams: %v", shortID, n, err)
		}
		return err
	})
	sm.CoalesceDelay = cfg.CoalesceDelay()
	sm.Reorder = true

	ups = upstream.New(cfg, func(f protocol.Frame) {
		switch f.Type {
		// --- Control ---
		case protocol.MsgPeerConn:
			peerID, iamToken, helperShortID, err := protocol.DecodePeerConn(f.Payload)
			if err != nil {
				log.Printf("[WARN] bad PEER_CONN: %v", err)
				return
			}
			if helperShortID == 0 {
				// Legacy / cloud function without multi-helper support. Treat
				// as helper shortID=1 so a single helper still works.
				helperShortID = 1
			}
			if ups.IsHelperStale(helperShortID, peerID) {
				log.Printf("[WARN] PEER_CONN with stale ID shortID=%d peerID=%s, ignoring", helperShortID, peerID)
				return
			}
			log.Printf("[INFO] PEER_CONN received: shortID=%d peerID=%s tokenLen=%d", helperShortID, peerID, len(iamToken))
			ups.SetHelper(helperShortID, peerID)
			if iamToken != "" {
				ups.SetIAMToken(iamToken)
			}
		case protocol.MsgPeerGone:
			shortID := protocol.DecodePeerGone(f.Payload)
			if shortID == 0 {
				// Legacy / all-peers-gone (e.g. cloud function couldn't tell us
				// which). Close everything.
				log.Printf("[INFO] PEER_GONE (all) received, closing %d streams", sm.Count())
				// Clear every helper slot.
				for sid := range ups.Helpers() {
					ups.RemoveHelper(sid)
				}
				sm.CloseAll()
			} else {
				old := ups.RemoveHelper(shortID)
				n := sm.CloseHelper(shortID)
				log.Printf("[INFO] PEER_GONE received: shortID=%d peerID=%s closed=%d streams", shortID, old, n)
			}
		case protocol.MsgPong:
			iamToken, err := protocol.DecodePong(f.Payload)
			if err != nil {
				log.Printf("[WARN] bad PONG: %v", err)
				return
			}
			log.Printf("[DEBUG] PONG received, tokenLen=%d", len(iamToken))
			ups.SetIAMToken(iamToken)
		case protocol.MsgPing:
			// We never answer PINGs (only the cloud function does). A stray PING
			// can still reach us if an older cloud function relays the peer's
			// keepalive instead of handling it; ignore it quietly instead of
			// logging it as an unknown frame.
			log.Printf("[DEBUG] ignoring stray PING")

		// --- Stream ---
		case protocol.MsgOpen, protocol.MsgData, protocol.MsgFin, protocol.MsgRst:
			if f.Type != protocol.MsgData {
				log.Printf("[INFO] %s received stream=%d seq=%d",
					map[byte]string{protocol.MsgOpen: "OPEN", protocol.MsgFin: "FIN", protocol.MsgRst: "RST"}[f.Type],
					f.StreamID, f.SeqID)
			}
			// Probe streams are entirely synthetic on the adapter side: we never
			// register a Stream, never dial a target, and we don't care about
			// in-order delivery of the helper's GET/FIN — so skip the reorder
			// machinery (which would otherwise leak a buffer entry per probe).
			if protocol.IsProbe(f.StreamID) {
				if f.Type == protocol.MsgOpen {
					go handleProbe(sm, f.StreamID)
				}
				// DATA/FIN/RST from the helper for this probe are silently absorbed.
				return
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

	// HTTP server for the conn-ids endpoint (path configurable via http.path).
	// Required — the cloud function calls it on cold start to recover state.
	{
		httpPath := cfg.HTTP.Path
		if httpPath == "" {
			httpPath = "/conn-ids"
		}
		if !strings.HasPrefix(httpPath, "/") {
			httpPath = "/" + httpPath
		}
		mux := http.NewServeMux()
		mux.HandleFunc(httpPath, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.Bridge.AuthToken)) != 1 {
				log.Printf("[WARN] %s unauthorized request from %s", httpPath, r.RemoteAddr)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// Cloud function may piggyback its current YC IAM token here so we
			// can refresh ours without a PING/PONG round trip. Allows us to keep
			// the periodic PING loop low-frequency / peer-gated.
			if iamToken := r.Header.Get("X-IAM-Token"); iamToken != "" {
				ups.SetIAMToken(iamToken)
				log.Printf("[INFO] %s refreshed IAM token from %s tokenLen=%d", httpPath, r.RemoteAddr, len(iamToken))
			}
			own := ups.OwnConnID()
			peer := ups.PeerConnID()
			helpers := ups.Helpers()
			if own == "" {
				log.Printf("[INFO] %s requested from %s but adapter not connected yet", httpPath, r.RemoteAddr)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			log.Printf("[INFO] %s requested from %s adapterConnId=%s helpers=%d", httpPath, r.RemoteAddr, own, len(helpers))
			// helpers field is the multi-helper map (preferred by the cloud
			// function on cold-start recovery). helperConnId is preserved for
			// compatibility with older cloud-function deployments that only
			// understand a single helper.
			helperList := make([]map[string]any, 0, len(helpers))
			for sid, cid := range helpers {
				helperList = append(helperList, map[string]any{
					"shortId": sid,
					"connId":  cid,
				})
			}
			resp := map[string]any{
				"adapterConnId": own,
				"helperConnId":  peer, // legacy single-helper compat
				"helpers":       helperList,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		})
		addr := fmt.Sprintf(":%d", cfg.HTTP.ListenPort)
		// This endpoint is exposed to the public internet (the cloud function
		// polls it on cold start). Set timeouts so slow or half-open clients
		// can't tie up connections indefinitely (Slowloris-style exhaustion).
		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
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

// handleProbe synthesises an HTTP/1.1 200 OK response without dialling any
// target. Triggered by an OPEN whose streamID has the PROBE bit set (see
// protocol.StreamProbeFlag). The probe round-trip exercises the full wsApi
// data path — helper → wsApi → adapter and adapter → wsApi → helper — so
// the helper can confirm bidirectional connectivity is actually working,
// independently of whether the adapter's configured target is reachable.
func handleProbe(sm *streams.Manager, streamID uint32) {
	log.Printf("[INFO] probe stream=%d: synthesising HTTP 200 OK (no target dial)", streamID)

	if err := sm.SendFrame(protocol.Frame{Type: protocol.MsgOpenOK, StreamID: streamID}); err != nil {
		log.Printf("[WARN] probe OPEN_OK send failed stream=%d: %v", streamID, err)
		return
	}

	body := "HTTP/1.1 200 OK\r\n" +
		"Server: bridge-to-freedom-adapter\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"OK"
	if err := sm.SendFrame(protocol.Frame{
		Type:     protocol.MsgData,
		StreamID: streamID,
		Payload:  []byte(body),
	}); err != nil {
		log.Printf("[WARN] probe DATA send failed stream=%d: %v", streamID, err)
		return
	}
	if err := sm.SendFrame(protocol.Frame{Type: protocol.MsgFin, StreamID: streamID}); err != nil {
		log.Printf("[WARN] probe FIN send failed stream=%d: %v", streamID, err)
		return
	}
	log.Printf("[INFO] probe stream=%d: response sent (%d bytes)", streamID, len(body))
}
