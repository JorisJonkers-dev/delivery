package gate_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

func scrape(t *testing.T, c gate.Cluster) (int, string, string) {
	t.Helper()
	srv, _ := serve(t, c)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+gate.PathMetrics, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	said, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(said)
}

func TestTheMetricsStackAloneScrapesTheGate(t *testing.T) {
	status, kind, body := scrape(t, auth())
	if status != http.StatusOK || !strings.HasPrefix(kind, "text/plain; version=0.0.4") {
		t.Fatalf("the metrics stack: %d %s", status, kind)
	}
	// Before its first round, the gate says it has never tended.
	if !strings.Contains(body, "\nrelease_gate_tended_timestamp_seconds 0\n") {
		t.Fatalf("body = %s", body)
	}

	stranger := auth()
	stranger.strangers = true
	if status, _, body := scrape(t, stranger); status != http.StatusForbidden || strings.TrimSpace(body) != "only the metrics stack scrapes the Release Gate" {
		t.Fatalf("a stranger: %d %s", status, body)
	}
	blind := auth()
	blind.away = "scrapers"
	if status, _, body := scrape(t, blind); status != http.StatusServiceUnavailable || strings.Contains(body, "away") {
		t.Fatalf("a gate that cannot tell: %d %s", status, body)
	}
}

func TestARoundOfTendingIsOneSeriesPerFinding(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	standing := gate.Standing{
		At: at,
		Held: []gate.Held{{
			Gated:   gate.Gated{Namespace: "auth-system", Application: "auth"},
			Serving: "sha256:a", Pinned: "sha256:b", Reason: gate.HeldAnalysis,
			Since: at.Add(-time.Hour), Deadline: 30 * time.Minute,
		}},
		Refused:      []gate.Refused{{Gated: gate.Gated{Namespace: "notes-system", Application: "notes"}, Tag: "v1", Condition: gate.UndoNonTransactional}},
		Unanswerable: []gate.Gated{{Namespace: "odd-system", Application: `a"b\c` + "\n"}},
	}

	got := gate.Exposition(standing)

	want := `# HELP release_gate_tended_timestamp_seconds When the gate last finished a round of tending; 0 before its first.
# TYPE release_gate_tended_timestamp_seconds gauge
release_gate_tended_timestamp_seconds 1793016000
# HELP release_gate_held_since_timestamp_seconds When the gate first saw a held release's pinned revision applied and not serving.
# TYPE release_gate_held_since_timestamp_seconds gauge
release_gate_held_since_timestamp_seconds{namespace="auth-system",application="auth",reason="analysis-failed",serving="sha256:a",pinned="sha256:b"} 1793012400
# HELP release_gate_held_deadline_seconds A held release's gate deadline: held past it, nothing moves it on its own.
# TYPE release_gate_held_deadline_seconds gauge
release_gate_held_deadline_seconds{namespace="auth-system",application="auth",reason="analysis-failed",serving="sha256:a",pinned="sha256:b"} 1800
# HELP release_gate_undo_refused A migration the gate will not undo, or could not, by the condition that held it back.
# TYPE release_gate_undo_refused gauge
release_gate_undo_refused{namespace="notes-system",application="notes",tag="v1",condition="non-transactional"} 1
# HELP release_gate_unanswerable An Application the gate could not read, so answers no for.
# TYPE release_gate_unanswerable gauge
release_gate_unanswerable{namespace="odd-system",application="a\"b\\c\n"} 1
`
	if got != want {
		t.Fatalf("exposition =\n%s\nwant\n%s", got, want)
	}
}
