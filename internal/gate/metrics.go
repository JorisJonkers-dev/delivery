package gate

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// PathMetrics is where the metrics stack scrapes what the last round of tending found
// (JorisJonkers-dev/delivery#22). It names namespaces, Applications, revisions and why a release
// is held, so it answers the metrics stack's pods alone, as the webhooks answer Flagger's.
const PathMetrics = "/metrics"

// refusedScrape is what anyone but the metrics stack is told.
const refusedScrape = "only the metrics stack scrapes the Release Gate"

// metricsRoute serves the last round of tending in Prometheus's text format: 403 to anyone but the
// metrics stack, and 503 where the gate cannot tell who that is.
func (g *Gate) metricsRoute(w http.ResponseWriter, r *http.Request, failed func(error)) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	scraper, err := from(r, "the metrics stack's", g.cluster.Scrapers)
	if err != nil {
		failed(err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "the gate could not tell who scrapes it\n")
		return
	}
	if !scraper {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, refusedScrape+"\n")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, Exposition(g.Standing())) //nolint:gosec // G705: Prometheus's text format, with nosniff
}

// Exposition is a round of tending as gauges, one series per finding, so an alert rule reads each:
// a held release by its reason, past its deadline or not; a migration left applied; an
// Application the gate could not read; and when the gate last finished a round.
func Exposition(s Standing) string {
	var out strings.Builder
	gauge := func(name, help string) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	series := func(name string, value float64, labels ...string) {
		out.WriteString(name)
		if len(labels) > 0 {
			pairs := make([]string, 0, len(labels)/2)
			for i := 0; i+1 < len(labels); i += 2 {
				pairs = append(pairs, labels[i]+`="`+escape(labels[i+1])+`"`)
			}
			out.WriteString("{" + strings.Join(pairs, ",") + "}")
		}
		out.WriteString(" " + strconv.FormatFloat(value, 'f', -1, 64) + "\n")
	}

	gauge("release_gate_tended_timestamp_seconds", "When the gate last finished a round of tending; 0 before its first.")
	tended := 0.0
	if !s.At.IsZero() {
		tended = float64(s.At.Unix())
	}
	series("release_gate_tended_timestamp_seconds", tended)

	gauge("release_gate_held_since_timestamp_seconds", "When the gate first saw a held release's pinned revision applied and not serving.")
	for _, h := range s.Held {
		series("release_gate_held_since_timestamp_seconds", float64(h.Since.Unix()),
			"namespace", h.Namespace, "application", h.Application, "reason", h.Reason, "serving", h.Serving, "pinned", h.Pinned)
	}
	gauge("release_gate_held_deadline_seconds", "A held release's gate deadline: held past it, nothing moves it on its own.")
	for _, h := range s.Held {
		series("release_gate_held_deadline_seconds", h.Deadline.Seconds(),
			"namespace", h.Namespace, "application", h.Application, "reason", h.Reason, "serving", h.Serving, "pinned", h.Pinned)
	}
	gauge("release_gate_undo_refused", "A migration the gate will not undo, or could not, by the condition that held it back.")
	for _, r := range s.Refused {
		series("release_gate_undo_refused", 1, "namespace", r.Namespace, "application", r.Application, "tag", r.Tag, "condition", r.Condition)
	}
	gauge("release_gate_unanswerable", "An Application the gate could not read, so answers no for.")
	for _, a := range s.Unanswerable {
		series("release_gate_unanswerable", 1, "namespace", a.Namespace, "application", a.Application)
	}
	return out.String()
}

// escape is a label value as the text format writes it.
func escape(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}
