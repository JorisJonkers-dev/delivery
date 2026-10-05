package collector_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/collector"
)

// clock is a time a test moves by hand.
type clock struct{ nanos atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *clock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

func fresh(t *testing.T, loop *collector.Loop) int {
	t.Helper()
	mux := http.NewServeMux()
	loop.Routes(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/fresh", nil))
	return recorder.Code
}

func TestTheLoopCapturesAtStartAndAtEveryInterval(t *testing.T) {
	ticks := make(chan time.Time)
	ran := make(chan struct{})
	var asked time.Duration
	capture := func(context.Context) (bool, error) {
		ran <- struct{}{}
		return false, nil
	}
	tick := func(d time.Duration) <-chan time.Time {
		asked = d
		return ticks
	}
	loop := collector.NewLoop(capture, 10*time.Minute, time.Now, tick, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	<-ran // at start, before any interval passed
	ticks <- time.Now()
	<-ran
	ticks <- time.Now()
	<-ran
	cancel()
	<-done
	if asked != 10*time.Minute {
		t.Fatalf("the loop asked for a tick every %s", asked)
	}
}

func TestACollectorIsFreshWhileACaptureSucceededWithinTwiceTheInterval(t *testing.T) {
	var at clock
	ticks := make(chan time.Time)
	// Each capture waits to be told how it ends, so the test decides when one finishes.
	results := make(chan error)
	capture := func(ctx context.Context) (bool, error) {
		select {
		case err := <-results:
			return err == nil, err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	loop := collector.NewLoop(capture, time.Minute, at.now, func(time.Duration) <-chan time.Time { return ticks },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	// ends one capture with err and lets the next begin. The loop takes a tick only after it
	// has recorded how the capture ended, so once the tick is taken the record is written, and
	// the loop is parked inside the next capture while the test moves the clock.
	ends := func(err error) {
		results <- err
		ticks <- at.now()
	}

	// A Collector that has only just been made has not failed yet.
	if !loop.Fresh() {
		t.Fatal("a new Collector is stale")
	}
	at.advance(2*time.Minute + time.Nanosecond)
	if loop.Fresh() || fresh(t, loop) != http.StatusServiceUnavailable {
		t.Fatal("a Collector that never captured within twice its interval is fresh")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	// The capture at start succeeds, so the Collector is fresh for twice the interval, to the nanosecond.
	ends(nil)
	at.advance(2 * time.Minute)
	if !loop.Fresh() || fresh(t, loop) != http.StatusOK {
		t.Fatal("a Collector is stale at exactly twice its interval")
	}
	// A capture that fails does not count: the mark stays where the last success left it.
	ends(errors.New("the cluster is away"))
	at.advance(time.Nanosecond)
	if loop.Fresh() || fresh(t, loop) != http.StatusServiceUnavailable {
		t.Fatal("a Collector whose captures fail stays fresh")
	}
	// And one that succeeds again is fresh again.
	ends(nil)
	if !loop.Fresh() {
		t.Fatal("a Collector that captured again is stale")
	}
	cancel()
	<-done
}
