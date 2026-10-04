package server_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/server"
)

func newServer() *server.Server {
	return server.New(slog.New(slog.DiscardHandler), "test", func(mux *http.ServeMux) {
		mux.HandleFunc("GET /own", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	})
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestProbesBeforeServing(t *testing.T) {
	h := newServer().Handler()
	cases := []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusServiceUnavailable},
		{"/nope", http.StatusNotFound},
		// The service's own routes sit on the same mux as the probes.
		{"/own", http.StatusTeapot},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := &recorder{header: http.Header{}}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			h.ServeHTTP(rec, req)
			if rec.code != tc.want {
				t.Fatalf("GET %s = %d, want %d", tc.path, rec.code, tc.want)
			}
		})
	}
}

func TestServeIsReadyThenDrainsOnCancel(t *testing.T) {
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- newServer().Serve(ctx, ln) }()

	url := "http://" + ln.Addr().String()
	waitFor(t, func() bool { return get(t, url+"/readyz") == http.StatusOK })
	if got := get(t, url+"/healthz"); got != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	ln := listen(t)
	_ = ln.Close()
	if err := newServer().Serve(t.Context(), ln); err == nil {
		t.Fatal("Serve on a closed listener returned nil")
	}
}

// recorder is a minimal http.ResponseWriter, so the probe table needs no httptest.
type recorder struct {
	header http.Header
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(code int)        { r.code = code }

func get(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
