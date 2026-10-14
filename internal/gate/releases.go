package gate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/deploykit/resolved"
)

// PathReleases is where the gate serves what it holds of every gated Application.
const PathReleases = "/releases"

// Release is one gated Application as the gate holds it: the members and the migration its
// release-gate inputs hand the gate, and what the gate recorded of it. It is what the estate
// dashboard shows of a release, served here so the dashboard reads no ConfigMap of its own
// (JorisJonkers-dev/estate-dashboard#15): the gate already reads both, and is the one writer of
// the record.
type Release struct {
	Namespace   string            `json:"namespace"`
	Application string            `json:"application"`
	Members     []string          `json:"members,omitempty"`
	Migration   *ReleaseMigration `json:"migration,omitempty"`
	// Serving, Pinned and Since are the gate's record: what serves, the revision applied and not
	// yet serving, and since when. Empty where it recorded nothing.
	Serving string     `json:"serving,omitempty"`
	Pinned  string     `json:"pinned,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
	// Unreadable says why the gate could not read the Application's inputs or record. The
	// fields that depend on what it could not read are then empty.
	Unreadable string `json:"unreadable,omitempty"`
}

// ReleaseMigration is the migration a release's inputs name.
type ReleaseMigration struct {
	Identity         string `json:"identity"`
	TestedAgainst    string `json:"testedAgainst,omitempty"`
	NonTransactional bool   `json:"nonTransactional"`
}

// Releases are what GET /releases answers.
type Releases struct {
	Releases []Release `json:"releases"`
	// Truncated is set where the cluster holds more gated Applications than one answer carries.
	Truncated bool `json:"truncated"`
}

// maxReleases is the most Applications one answer carries, so no answer grows with what anyone
// can create.
const maxReleases = 1000

// Why a release cannot be read, as GET /releases says it.
const (
	unreadableNoInputs = "no release-gate inputs"
	unreadableInputs   = "release-gate inputs that do not parse"
	unreadableRecord   = "a release record the gate did not write for it"
)

// Releases reads every gated Application, ordered by namespace and Application. An Application
// whose inputs or record do not read is reported as such and keeps no other from being read; a
// cluster that does not answer is an error.
func (g *Gate) Releases(ctx context.Context) (Releases, error) {
	gated, err := g.cluster.Applications(ctx)
	if err != nil {
		return Releases{}, fmt.Errorf("gate: list the gated Applications: %w", err)
	}
	slices.SortFunc(gated, func(a, b Gated) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Application, b.Application))
	})
	out := Releases{Releases: []Release{}}
	if len(gated) > maxReleases {
		gated, out.Truncated = gated[:maxReleases], true
	}
	for _, a := range gated {
		release, err := g.release(ctx, a)
		if err != nil {
			return Releases{}, err
		}
		out.Releases = append(out.Releases, release)
	}
	return out, nil
}

func (g *Gate) release(ctx context.Context, a Gated) (Release, error) {
	release := Release{Namespace: a.Namespace, Application: a.Application}
	raw, err := g.cluster.Inputs(ctx, a.Namespace, a.Application)
	switch {
	case errors.Is(err, ErrNoInputs):
		release.Unreadable = unreadableNoInputs
		return release, nil
	case err != nil:
		return Release{}, fmt.Errorf("gate: read the release-gate inputs of %s/%s: %w", a.Namespace, a.Application, err)
	}
	var inputs resolved.ReleaseGate
	if json.Unmarshal([]byte(raw), &inputs) != nil {
		release.Unreadable = unreadableInputs
		return release, nil //nolint:nilerr // inputs that do not parse are the release's own state, not the cluster's failure
	}
	for _, m := range inputs.Members {
		release.Members = append(release.Members, m.Process)
	}
	if m := inputs.Migration; m != nil {
		release.Migration = &ReleaseMigration{Identity: m.Identity, NonTransactional: m.NonTransactional}
		if m.TestedAgainst != nil {
			release.Migration.TestedAgainst = *m.TestedAgainst
		}
	}

	record, err := g.cluster.Record(ctx, a.Namespace, a.Application)
	switch {
	case errors.Is(err, ErrNoRecord):
		return release, nil
	case errors.Is(err, ErrForeignRecord):
		release.Unreadable = unreadableRecord
		return release, nil
	case err != nil:
		return Release{}, fmt.Errorf("gate: read the release record of %s/%s: %w", a.Namespace, a.Application, err)
	}
	release.Serving, release.Pinned = record.Serving, record.Pinned
	if !record.Since.IsZero() {
		since := record.Since.UTC()
		release.Since = &since
	}
	return release, nil
}

// servedFor is how long one read of the releases answers GET /releases. The route checks no
// caller, and a read is a list and two gets per Application through the client the webhooks
// share, so asking it often must not cost the cluster more: at most one read per period, and one
// at a time. A screen that refreshes is no slower than this anyway.
const servedFor = 15 * time.Second

// served is the last read of the releases, and when it was made.
type served struct {
	mu       sync.Mutex
	now      func() time.Time
	at       time.Time
	releases Releases
}

// served returns the releases as last read, reading them again where that read is older than
// servedFor. Callers wait on one read rather than each making their own; a read that fails is
// not kept, so the next caller tries again.
func (g *Gate) servedReleases(ctx context.Context) (Releases, error) {
	g.served.mu.Lock()
	defer g.served.mu.Unlock()
	now := g.served.now()
	if !g.served.at.IsZero() && now.Sub(g.served.at) < servedFor {
		return g.served.releases, nil
	}
	releases, err := g.Releases(ctx)
	if err != nil {
		return Releases{}, err
	}
	g.served.at, g.served.releases = now, releases
	return releases, nil
}

// releasesRoute answers GET /releases. It answers anyone the gate's policy admits: what it says
// is what the dashboard shows, and no answer of it starts or holds anything.
func (g *Gate) releasesRoute(w http.ResponseWriter, r *http.Request, failed func(error)) {
	releases, err := g.servedReleases(r.Context())
	if err != nil {
		failed(err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("the gate could not read the cluster\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(releases)
}
