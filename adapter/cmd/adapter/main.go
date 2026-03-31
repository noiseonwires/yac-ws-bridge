package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bridge-to-freedom/adapter/internal/config"
	"github.com/bridge-to-freedom/adapter/internal/handler"
	"github.com/bridge-to-freedom/adapter/internal/protocol"
	"github.com/bridge-to-freedom/adapter/internal/upstream"
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

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("[INFO] shutting down")
		cancel()
	}()

	h := handler.New(cfg)
	ups := upstream.New(cfg, h)

	// Wakeup HTTP server: receives protocol frames via POST (same as upstream WS)
	if cfg.Wakeup.ListenPort > 0 {
		prefix := strings.TrimRight(cfg.Wakeup.PathPrefix, "/")
		mux := http.NewServeMux()
		mux.HandleFunc(prefix+"/upstream-id", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token != cfg.Bridge.AuthToken {
				log.Println("[WARN] /upstream-id unauthorized request")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			connID := ups.UpstreamConnID()
			if connID == "" {
				log.Println("[INFO] /upstream-id requested, no upstream connected")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			log.Printf("[INFO] /upstream-id requested, returning connID=%s", connID)
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(connID))
		})
		mux.HandleFunc(prefix+"/proxy", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token != cfg.Bridge.AuthToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var proxyReq struct {
				Method      string            `json:"method"`
				Path        string            `json:"path"`
				QueryString map[string]string `json:"queryString"`
				Headers     map[string]string `json:"headers"`
				Body        string            `json:"body"`
				IsBase64    bool              `json:"isBase64Encoded"`
			}
			if err := json.Unmarshal(body, &proxyReq); err != nil {
				log.Printf("[WARN] bad proxy request err=%v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Build target URL (ws:// -> http://, wss:// -> https://)
			targetBase := cfg.Target.URL
			targetBase = strings.Replace(targetBase, "wss://", "https://", 1)
			targetBase = strings.Replace(targetBase, "ws://", "http://", 1)
			targetURL := targetBase + proxyReq.Path
			if len(proxyReq.QueryString) > 0 {
				qs := url.Values{}
				for k, v := range proxyReq.QueryString {
					qs.Set(k, v)
				}
				targetURL += "?" + qs.Encode()
			}

			var reqBody io.Reader
			if proxyReq.Body != "" {
				if proxyReq.IsBase64 {
					decoded, err := base64.StdEncoding.DecodeString(proxyReq.Body)
					if err != nil {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					reqBody = bytes.NewReader(decoded)
				} else {
					reqBody = strings.NewReader(proxyReq.Body)
				}
			}

			method := proxyReq.Method
			if method == "" {
				method = http.MethodGet
			}
			proxyHTTP, err := http.NewRequestWithContext(r.Context(), method, targetURL, reqBody)
			if err != nil {
				log.Printf("[ERROR] proxy: create request err=%v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for k, v := range proxyReq.Headers {
				low := strings.ToLower(k)
				if low == "authorization" || low == "host" || low == "content-length" || low == "connection" {
					continue
				}
				proxyHTTP.Header.Set(k, v)
			}

			resp, err := http.DefaultClient.Do(proxyHTTP)
			if err != nil {
				log.Printf("[ERROR] proxy: target err=%v", err)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"statusCode":502,"body":"target unreachable"}`))
				return
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
			if err != nil {
				log.Printf("[ERROR] proxy: read response err=%v", err)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"statusCode":502,"body":"read error"}`))
				return
			}

			respHeaders := make(map[string]string)
			for k := range resp.Header {
				respHeaders[k] = resp.Header.Get(k)
			}

			result := struct {
				StatusCode      int               `json:"statusCode"`
				Headers         map[string]string `json:"headers"`
				Body            string            `json:"body"`
				IsBase64Encoded bool              `json:"isBase64Encoded"`
			}{
				StatusCode:      resp.StatusCode,
				Headers:         respHeaders,
				Body:            base64.StdEncoding.EncodeToString(respBody),
				IsBase64Encoded: true,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)
			log.Printf("[INFO] proxy: %s %s -> %d", method, proxyReq.Path, resp.StatusCode)
		})
		mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token != cfg.Bridge.AuthToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f, err := protocol.Decode(body)
			if err != nil {
				log.Printf("[DEBUG] bad wakeup frame err=%v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			h.HandleFrame(f)
			// Trigger upstream connection if not connected
			ups.EnsureConnected(ctx)
			// Return upstream connection ID if available
			if connID := ups.UpstreamConnID(); connID != "" {
				w.Header().Set("Content-Type", "text/plain")
				w.Write([]byte(connID))
			} else {
				w.WriteHeader(http.StatusOK)
			}
		})
		addr := fmt.Sprintf(":%d", cfg.Wakeup.ListenPort)
		srv := &http.Server{Addr: addr, Handler: mux}
		go func() {
			log.Printf("[INFO] wakeup HTTP server starting addr=%s", addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("wakeup server: %v", err)
			}
		}()
		go func() { <-ctx.Done(); srv.Close() }()
	}

	log.Printf("[INFO] adapter starting bridge=%s target=%s", cfg.Bridge.URL, cfg.Target.URL)
	ups.Run(ctx)
}
