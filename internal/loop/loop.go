// Package loop runs one pass when a service starts and then once every interval, and answers
// whether a pass succeeded lately: the Collector's captures, and the Reporter's reads.
package loop

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Loop runs a pass when it starts and then once every interval, until it is told to stop. The
// model has no schedule to run one on, so each service keeps its own
// (deploy-kit spec/v1/20-resolved-deployment.md#the-collector).
type Loop struct {
	pass     func(context.Context) (bool, error)
	interval time.Duration
	now      func() time.Time
	tick     func(time.Duration) <-chan time.Time
	log      *slog.Logger
	// succeeded is when a pass last succeeded, and when the Loop was made until one has.
	succeeded atomic.Int64
}

// New returns a Loop that runs pass every interval. tick hands it the channel an interval
// arrives on, so a test can deliver one without waiting for it, and a service can deliver one
// early.
func New(pass func(context.Context) (bool, error), interval time.Duration, now func() time.Time,
	tick func(time.Duration) <-chan time.Time, log *slog.Logger,
) *Loop {
	l := &Loop{pass: pass, interval: interval, now: now, tick: tick, log: log}
	l.succeeded.Store(now().UnixNano())
	return l
}

// Run runs passes until ctx is cancelled. A pass that fails is logged and the next still runs:
// the cluster or GitHub being away for one interval is not a reason to stop looking.
func (l *Loop) Run(ctx context.Context) {
	ticks := l.tick(l.interval)
	for {
		if _, err := l.pass(ctx); err != nil {
			l.log.ErrorContext(ctx, "a pass failed", "error", err)
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

// Fresh reports whether a pass succeeded within twice the interval. A Collector commits only
// when a fact changed, and a Reporter writes only when a stage did, so one that stopped and a
// cluster that stayed still look the same from outside; this is what tells them apart.
func (l *Loop) Fresh() bool {
	return l.now().Sub(time.Unix(0, l.succeeded.Load())) <= 2*l.interval
}

// Routes mounts the freshness probe, which is the service's liveness: one that stopped
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
