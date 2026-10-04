// Package server is the HTTP surface of the service: liveness and readiness probes, and a
// graceful drain when its context is cancelled.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	readHeaderTimeout = 5 * time.Second
	shutdownGrace     = 10 * time.Second
)

// Server serves /healthz and /readyz, and the service's own routes beside them.
type Server struct {
	logger  *slog.Logger
	version string
	routes  func(*http.ServeMux)
	ready   atomic.Bool
}

// New returns a Server that logs to logger and reports version at startup. routes mounts the
// service's own routes on the mux the probes are on.
func New(logger *slog.Logger, version string, routes func(*http.ServeMux)) *Server {
	return &Server{logger: logger, version: version, routes: routes}
}

// Handler returns the routes. Liveness never depends on readiness.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		plain(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			plain(w, http.StatusServiceUnavailable, "not ready")
			return
		}
		plain(w, http.StatusOK, "ready")
	})
	s.routes(mux)
	return mux
}

// Serve accepts on ln until ctx is cancelled, then fails readiness and drains in-flight
// requests for up to the shutdown grace period.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: readHeaderTimeout}

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()
	s.ready.Store(true)
	s.logger.Info("listening", "addr", ln.Addr().String(), "version", s.version)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	s.ready.Store(false)
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(drain); err != nil {
		return err
	}
	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	s.logger.Info("stopped")
	return nil
}

func plain(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body+"\n")
}
