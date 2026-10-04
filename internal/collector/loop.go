package collector

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Loop captures when it starts and then once every interval, until it is told to stop. The
// model has no schedule to run a capture on, so the Collector keeps its own
// (deploy-kit spec/v1/20-resolved-deployment.md#the-collector).
type Loop struct {
	capture  func(context.Context) (bool, error)
	interval time.Duration
	now      func() time.Time
	tick     func(time.Duration) <-chan time.Time
	log      *slog.Logger
	// succeeded is when a capture last succeeded, and when the Loop was made until one has.
	succeeded atomic.Int64
}

// NewLoop returns a Loop that runs capture every interval. tick hands it the channel an
// interval arrives on, so a test can deliver one without waiting for it.
func NewLoop(capture func(context.Context) (bool, error), interval time.Duration, now func() time.Time,
	tick func(time.Duration) <-chan time.Time, log *slog.Logger,
) *Loop {
	l := &Loop{capture: capture, interval: interval, now: now, tick: tick, log: log}
	l.succeeded.Store(now().UnixNano())
	return l
}

// Run captures until ctx is cancelled. A capture that fails is logged and the next still runs:
// the cluster or GitHub being away for one interval is not a reason to stop looking.
func (l *Loop) Run(ctx context.Context) {
	ticks := l.tick(l.interval)
	for {
		if _, err := l.capture(ctx); err != nil {
			l.log.ErrorContext(ctx, "a capture failed", "error", err)
		} else {
			l.succeeded.Store(l.now().UnixNano())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// Fresh reports whether a capture succeeded within twice the interval. A Collector commits only
// when a fact changed, so one that stopped and a cluster that stayed still look the same in the
// Estate repository; this is what tells them apart.
func (l *Loop) Fresh() bool {
	return l.now().Sub(time.Unix(0, l.succeeded.Load())) <= 2*l.interval
}

// Routes mounts the freshness probe, which is the Collector's liveness: a Collector that stopped
// succeeding is restarted, and shows as restarts.
func (l *Loop) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /fresh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !l.Fresh() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "stale\n")
			return
		}
		_, _ = io.WriteString(w, "fresh\n")
	})
}
