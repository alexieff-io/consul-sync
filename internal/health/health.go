package health

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server serves health check and metrics endpoints.
type Server struct {
	addr   string
	ready  atomic.Bool
	server *http.Server
}

// NewServer creates a new health/metrics server.
func NewServer(addr string) *Server {
	return &Server{addr: addr}
}

// SetReady marks the server as ready (called after first successful sync).
func (s *Server) SetReady() {
	s.ready.Store(true)
}

// ListenAndServe starts the HTTP server for health checks and metrics.
func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.ready.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	})

	mux.Handle("GET /metrics", promhttp.Handler())

	s.server = &http.Server{Addr: s.addr, Handler: mux}
	return s.server.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}
