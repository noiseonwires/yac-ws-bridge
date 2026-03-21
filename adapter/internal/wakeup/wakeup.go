package wakeup

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/bridge-to-freedom/adapter/internal/handler"
	"github.com/bridge-to-freedom/adapter/internal/protocol"
	"github.com/bridge-to-freedom/adapter/internal/upstream"
)

// Server handles HTTP POST wake-up requests from the bridge.
type Server struct {
	Handler   *handler.Handler
	Upstream  *upstream.Upstream
	AuthToken string
	Ctx       context.Context
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token != s.AuthToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	frame, err := protocol.Decode(body)
	if err != nil {
		log.Printf("[WARN] bad frame in wakeup POST err=%v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.Handler.HandleFrame(frame)
	s.Upstream.EnsureConnected(s.Ctx)

	w.WriteHeader(http.StatusOK)
}
