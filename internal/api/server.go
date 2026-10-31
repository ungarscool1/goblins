// Package api exposes the HTTP surface of the control plane.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Check is a named readiness probe.
type Check struct {
	Name string
	Run  func(ctx context.Context) error
}

// Server hosts health endpoints and any mounted routes.
type Server struct {
	mux     *http.ServeMux
	log     *slog.Logger
	checks  []Check
	version string
	ready   atomic.Bool
}

// NewServer builds a Server. Readiness stays false until SetReady(true).
func NewServer(log *slog.Logger, version string, checks []Check) *Server {
	s := &Server{mux: http.NewServeMux(), log: log, checks: checks, version: version}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)
	return s
}

// SetReady flips the readiness gate, e.g. false while shutting down.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Handle mounts an additional route.
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	results := map[string]string{}
	ready := s.ready.Load()
	if !ready {
		results["startup"] = "not ready"
	}
	for _, c := range s.checks {
		if err := c.Run(ctx); err != nil {
			// Details stay in logs: this endpoint is unauthenticated.
			s.log.Warn("readiness check failed", "check", c.Name, "err", err)
			results[c.Name] = "failed"
			ready = false
		} else {
			results[c.Name] = "ok"
		}
	}
	code, status := http.StatusOK, "ready"
	if !ready {
		code, status = http.StatusServiceUnavailable, "not ready"
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": results})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve runs the HTTP server on ln until ctx is cancelled, then drains
// in-flight requests for at most shutdownTimeout.
func (s *Server) Serve(ctx context.Context, ln net.Listener, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 16,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.SetReady(false)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(sctx)
	if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) && err == nil {
		err = serveErr
	}
	return err
}
