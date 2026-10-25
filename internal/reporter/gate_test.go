package reporter_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp/githubtest"
	"github.com/JorisJonkers-dev/delivery/internal/reporter"
)

// gated is a Release Gate that answers what a test sets.
type gated struct {
	releases gate.Releases
	err      error
}

func (g *gated) Releases(context.Context) (gate.Releases, error) { return g.releases, g.err }

func TestARolloutEndsOnlyOnceTheGateHasWarmedItsRelease(t *testing.T) {
	github := githubtest.New(t, "")
	u := unit(pinned, of("notes", "notes-api"))
	deployment := deploy(github, u)
	g := &gated{err: errors.New("the gate is away")}
	r := newReporter(t, github, &cluster{units: []reporter.Unit{u}})
	r.Gate = g

	pass := func() {
		t.Helper()
		if _, err := r.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	pass()
	g.err = nil
	g.releases = gate.Releases{Releases: []gate.Release{
		{Namespace: "notes-system", Application: "notes", Warmup: &gate.Warmup{Revision: "sha256:r2"}},
		{Namespace: "other-system", Application: "other"},
	}}
	pass()
	pass()

	equal(t, states(github.Statuses(deployment.ID)), []string{
		"in_progress: notes 1.4.2 pinned",
		"in_progress: promoted; the Release Gate is warming up notes",
		"success: notes 1.4.2 promoted and warmed up",
	})
}

func TestTheGatesReleasesAreReadFromItsEndpoint(t *testing.T) {
	answers := map[string]func(http.ResponseWriter){
		"/ok/releases": func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"releases":[{"namespace":"notes-system","application":"notes","warmup":{"revision":"sha256:r2","failed":"x"}}],"truncated":false}`)
		},
		"/down/releases":    func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) },
		"/garbled/releases": func(w http.ResponseWriter) { _, _ = io.WriteString(w, "not json") },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		answers[r.URL.Path](w)
	}))
	t.Cleanup(srv.Close)
	at := func(path string) reporter.GateReleases {
		return reporter.GateReleases{URL: srv.URL + path, HTTP: &http.Client{Timeout: time.Second}}
	}

	got, err := at("/ok").Releases(t.Context())
	if err != nil || len(got.Releases) != 1 || got.Releases[0].Warmup == nil || *got.Releases[0].Warmup != (gate.Warmup{Revision: "sha256:r2", Failed: "x"}) {
		t.Fatalf("releases = %+v, %v", got, err)
	}
	for _, path := range []string{"/down", "/garbled"} {
		if _, err := at(path).Releases(t.Context()); err == nil {
			t.Fatalf("%s read as releases", path)
		}
	}
	if _, err := (reporter.GateReleases{URL: "http://a b", HTTP: http.DefaultClient}).Releases(t.Context()); err == nil {
		t.Fatal("a URL that does not parse was requested")
	}
	srv.Close()
	if _, err := at("/ok").Releases(t.Context()); err == nil {
		t.Fatal("a gate that is gone answered")
	}
}
