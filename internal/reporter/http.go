package reporter

import (
	"io"
	"net/http"
	"time"
)

// eventLimit is the most of an event's body the Reporter reads; it reads it only to drain it.
const eventLimit = 64 << 10

// Prompts turns Flux's events into early passes. An event is a prompt to look, never evidence
// (deploy-kit spec/v1/55-delivery.md#rollouts): the route is unsigned, so the Reporter takes
// nothing from the event at all, not even which unit it names, and reads every unit again.
type Prompts struct {
	prompted chan struct{}
}

// NewPrompts returns Prompts with room for one pass asked and not yet taken: any number of events
// that arrive while a pass is waiting ask for the same one.
func NewPrompts() *Prompts {
	return &Prompts{prompted: make(chan struct{}, 1)}
}

// Routes mounts the route Flux's Provider posts every event to.
func (p *Prompts) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /flux", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, eventLimit))
		select {
		case p.prompted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})
}

// Every returns the tick a Loop runs on: once every interval, and once more for each prompt.
func (p *Prompts) Every(interval time.Duration) <-chan time.Time {
	ticks := make(chan time.Time)
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case at := <-ticker.C:
				ticks <- at
			case <-p.prompted:
				ticks <- time.Now()
			}
		}
	}()
	return ticks
}
