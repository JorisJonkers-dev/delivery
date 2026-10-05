package gate_test

import (
	"context"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

const (
	// auth's inputs with the migration the render hands the gate: proven against `earlier`.
	migratingInputs = `{"analysis":{"interval":"30s","iterations":4,"threshold":3},"deadline":"1800s","endpoint":"http://release-gate.delivery-system.svc.cluster.local:8080","members":[{"process":"auth-api","readiness":{"path":"/ready","port":8081}},{"process":"auth-ui","readiness":{"path":"/","port":8080}}],"migration":{"identity":"auth-migration","nonTransactional":false,"testedAgainst":"` + earlier + `"}}`
	// The same, for a first release: proven against nothing, with no down to return to.
	firstInputs = `{"analysis":{"interval":"30s","iterations":4,"threshold":3},"deadline":"1800s","endpoint":"http://release-gate.delivery-system.svc.cluster.local:8080","members":[{"process":"auth-api","readiness":{"path":"/ready","port":8081}},{"process":"auth-ui","readiness":{"path":"/","port":8080}}],"migration":{"identity":"auth-migration","nonTransactional":false}}`
	upJob       = "auth-migration-9d2c4e6a8b0d"
	downJob     = "auth-migration-down-9d2c4e6a8b0d"
)

var (
	quiet = slog.New(slog.DiscardHandler)
	// first is when the gate first saw the revision under release, and now a round after it.
	first = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	now   = first.Add(time.Minute)
	app   = gate.Gated{Namespace: "auth-system", Application: "auth"}
)

// serving is the record of auth serving `earlier`, with the digests its primaries had then.
func serving() *gate.Record {
	return &gate.Record{Serving: earlier, Promoted: map[string]string{"auth-api": "a1", "auth-ui": "u1"}}
}

// releasing is a release of auth's API under way over what serving() records: the API's
// Deployment has changed and the UI's has not, and both Jobs are applied suspended.
func releasing() *cluster {
	c := auth()
	c.inputs = migratingInputs
	c.canaries = map[string]gate.Canary{
		"auth-api": of("Progressing", "a2", "a1", 0),
		"auth-ui":  of("Succeeded", "u1", "u1", 0),
	}
	c.changed = map[string]bool{"auth-api": true}
	c.pods = map[string][]gate.Pod{}
	c.record = serving()
	c.jobs = []gate.Job{
		{Name: upJob, Component: "auth-migration", Suspended: true},
		{Name: downJob, Component: "auth-migration", Suspended: true},
	}
	return c
}

// failedRelease is that release after its migration ran and Flagger failed the API: its new
// copy is gone, and the gate first saw the revision before the failure.
func failedRelease() *cluster {
	c := releasing()
	failed := of("Failed", "a2", "a1", 2)
	failed.Transitioned = first.Add(30 * time.Second)
	c.canaries["auth-api"] = failed
	c.jobs[0] = gate.Job{Name: upJob, Component: "auth-migration", Complete: true}
	c.record.Pinned, c.record.Since = revision, first
	return c
}

func tend(t *testing.T, c *cluster) gate.Standing {
	t.Helper()
	g := gate.New(c)
	if err := g.Tend(t.Context(), now, quiet); err != nil {
		t.Fatal(err)
	}
	return g.Standing()
}

func TestTheGateRecordsWhatAnApplicationServesAndWritesItOnce(t *testing.T) {
	c := auth()
	c.canaries = map[string]gate.Canary{
		"auth-api": of("Succeeded", "a2", "a2", 4),
		"auth-ui":  of("Initialized", "u1", "u1", 0),
	}
	c.record = &gate.Record{Serving: earlier, Promoted: map[string]string{"auth-api": "a1", "auth-ui": "u1"}, Pinned: revision, Since: first}

	found := tend(t, c)

	want := gate.Record{Serving: revision, Promoted: map[string]string{"auth-api": "a2", "auth-ui": "u1"}}
	if len(c.written) != 1 || c.written[0].Serving != want.Serving || !maps.Equal(c.written[0].Promoted, want.Promoted) || c.written[0].Pinned != "" || !c.written[0].Since.IsZero() {
		t.Fatalf("written = %+v", c.written)
	}
	if !found.At.Equal(now) || found.Held != nil || found.Refused != nil || found.Unanswerable != nil {
		t.Fatalf("standing = %+v", found)
	}
	// What it holds already is not written again.
	tend(t, c)
	if len(c.written) != 1 || len(c.started) != 0 {
		t.Fatalf("a second round wrote %+v and started %v", c.written, c.started)
	}
}

func TestAnApplicationServesOnlyWhenEveryPrimaryRunsWhatTheRenderHolds(t *testing.T) {
	cases := map[string]func(*cluster){
		"a member still analysed":            func(c *cluster) { c.canaries["auth-api"] = of("Progressing", "a2", "a1", 1) },
		"a member whose Deployment changed":  func(c *cluster) { c.changed["auth-ui"] = true },
		"a member Flagger has not caught up": func(c *cluster) { c.canaries["auth-ui"] = of("Succeeded", "u2", "u1", 0) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			c := auth()
			c.canaries = map[string]gate.Canary{"auth-api": of("Succeeded", "a2", "a2", 4), "auth-ui": of("Succeeded", "u1", "u1", 0)}
			edit(c)

			tend(t, c)

			// It notes the release it sees under way, and records nothing as serving.
			if len(c.written) != 1 || c.written[0].Serving != "" || c.written[0].Pinned != revision || !c.written[0].Since.Equal(now) {
				t.Fatalf("written = %+v", c.written)
			}
		})
	}
}

func TestAMigrationStartsOnlyWhileItsProofHolds(t *testing.T) {
	stale := []gate.Held{{Gated: app, Serving: "", Pinned: revision, Reason: gate.HeldStaleProof}}
	servingOther := func(c *cluster) { c.record.Serving = "sha256:ffff" }
	cases := map[string]struct {
		edit    func(*cluster)
		started []string
		held    []gate.Held
	}{
		"every primary runs the revision the proof ran against": {func(*cluster) {}, []string{upJob}, nil},
		"another release landed in between": {
			servingOther, nil,
			[]gate.Held{{Gated: app, Serving: "sha256:ffff", Pinned: revision, Reason: gate.HeldStaleProof}},
		},
		"a primary moved since the serving revision was recorded": {
			func(c *cluster) { c.canaries["auth-ui"] = of("Succeeded", "u1", "u9", 0); c.changed["auth-ui"] = true }, nil,
			[]gate.Held{{Gated: app, Serving: earlier, Pinned: revision, Reason: gate.HeldStaleProof}},
		},
		"a proof against a revision, and nothing recorded as serving": {func(c *cluster) { c.record = nil }, nil, stale},
		"a first release, with nothing serving":                       {func(c *cluster) { c.inputs = firstInputs; c.record = nil }, []string{upJob}, nil},
		"a proof against nothing, while something serves": {
			func(c *cluster) { c.inputs = firstInputs }, nil,
			[]gate.Held{{Gated: app, Serving: earlier, Pinned: revision, Reason: gate.HeldStaleProof}},
		},
		// A member the record does not know is new: it has no primary that could run anything older.
		"a member added in this release": {func(c *cluster) { delete(c.record.Promoted, "auth-api") }, []string{upJob}, nil},
		// A member that left the Application is no primary of it.
		"a member the record knows and the release dropped": {func(c *cluster) { c.record.Promoted["auth-worker"] = "w1" }, []string{upJob}, nil},
		"a migration already started":                       {func(c *cluster) { c.jobs[0].Suspended = false }, nil, nil},
		"a migration already complete":                      {func(c *cluster) { c.jobs[0].Complete = true }, nil, nil},
		"a migration not applied yet":                       {func(c *cluster) { c.jobs = nil }, nil, nil},
		"an Application with no migration":                  {func(c *cluster) { c.inputs = authInputs }, nil, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := releasing()
			tc.edit(c)

			found := tend(t, c)

			if !slices.Equal(c.started, tc.started) || !slices.Equal(found.Held, tc.held) || found.Refused != nil {
				t.Fatalf("started %v, standing %+v", c.started, found)
			}
		})
	}
}

func TestAStaleProofLeavesTheReleaseUntouched(t *testing.T) {
	c := releasing()
	c.record.Serving = "sha256:ffff"

	found := tend(t, c)

	// Nothing is started and nothing is undone: the schema is as the serving version left it.
	if c.started != nil || found.Refused != nil || len(found.Held) != 1 || found.Held[0].Reason != gate.HeldStaleProof {
		t.Fatalf("started %v, standing %+v", c.started, found)
	}
}

func TestTheDownStartsOnlyUnderEveryOneOfItsConditions(t *testing.T) {
	analysis := []gate.Held{{Gated: app, Serving: earlier, Pinned: revision, Reason: gate.HeldAnalysis}}
	refused := func(condition string) []gate.Refused {
		return []gate.Refused{{Gated: app, Tag: "0a1b2c3d4e5f", Condition: condition}}
	}
	cases := map[string]struct {
		edit    func(*cluster)
		started []string
		held    []gate.Held
		refused []gate.Refused
	}{
		"held, every new copy gone, every primary on the proven revision, transactional": {func(*cluster) {}, []string{downJob}, analysis, nil},
		"the migration itself failed": {
			func(c *cluster) {
				c.canaries["auth-api"] = of("Progressing", "a2", "a1", 0)
				c.jobs[0] = gate.Job{Name: upJob, Component: "auth-migration", Failed: true}
			},
			[]string{downJob},
			[]gate.Held{{Gated: app, Serving: earlier, Pinned: revision, Reason: gate.HeldMigration}},
			nil,
		},
		"not held: the release is still under way": {
			func(c *cluster) { c.canaries["auth-api"] = of("Progressing", "a2", "a1", 2) }, nil, nil, nil,
		},
		// Flagger's status carries no revision: a failure from before this revision was applied is the release before's.
		"a failure Flagger recorded before the gate saw this revision": {
			func(c *cluster) {
				failed := c.canaries["auth-api"]
				failed.Transitioned = first
				c.canaries["auth-api"] = failed
			}, nil, nil, nil,
		},
		"a member's new copy still runs": {
			func(c *cluster) { c.pods["auth-ui"] = []gate.Pod{{Name: "auth-ui-5b8f-y", Ready: true}} },
			nil, analysis, refused(gate.UndoNewCopyRunning),
		},
		"a member was promoted": {
			func(c *cluster) { c.canaries["auth-ui"] = of("Succeeded", "u2", "u2", 4); c.changed["auth-ui"] = false },
			nil, analysis, refused(gate.UndoPrimariesMoved),
		},
		"the Application serves another revision than the proven one": {
			func(c *cluster) { c.record.Serving = "sha256:ffff" },
			nil,
			[]gate.Held{{Gated: app, Serving: "sha256:ffff", Pinned: revision, Reason: gate.HeldAnalysis}},
			refused(gate.UndoPrimariesMoved),
		},
		"the release holds a changeset that cannot run in a transaction": {
			func(c *cluster) {
				c.inputs = strings.Replace(migratingInputs, `"nonTransactional":false`, `"nonTransactional":true`, 1)
			},
			nil, analysis, refused(gate.UndoNonTransactional),
		},
		"a first release, which has no down": {
			func(c *cluster) {
				c.inputs = firstInputs
				c.jobs = c.jobs[:1]
				c.record.Serving, c.record.Promoted = "", nil
			},
			nil,
			[]gate.Held{{Gated: app, Serving: "", Pinned: revision, Reason: gate.HeldAnalysis}},
			[]gate.Refused{{Gated: app, Condition: gate.UndoFirstRelease}},
		},
		"a down that failed": {
			func(c *cluster) { c.jobs[1] = gate.Job{Name: downJob, Component: "auth-migration", Failed: true} },
			nil, analysis, refused(gate.UndoFailed),
		},
		"a down already started": {func(c *cluster) { c.jobs[1].Suspended = false }, nil, analysis, nil},
		"a down that completed":  {func(c *cluster) { c.jobs[1].Complete = true }, nil, analysis, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := failedRelease()
			tc.edit(c)

			found := tend(t, c)

			if !slices.Equal(c.started, tc.started) || !slices.Equal(found.Held, tc.held) || !slices.Equal(found.Refused, tc.refused) {
				t.Fatalf("started %v, standing %+v", c.started, found)
			}
		})
	}
}

func TestTendingFailsClosed(t *testing.T) {
	unanswerable := []gate.Gated{app}
	for _, away := range []string{"inputs", "canary", "record", "serves", "jobs", "pods", "start", "set-record"} {
		t.Run(away+" away", func(t *testing.T) {
			c := failedRelease()
			switch away {
			case "set-record":
				// The one write a held release still makes is of a revision it has not noted.
				c.record.Pinned = ""
			case "serves":
				// The Deployments are compared only once Flagger records every member as settled.
				c.canaries["auth-api"] = of("Succeeded", "a2", "a2", 4)
			}
			c.away = away

			found := tend(t, c)

			if c.started != nil || !slices.Equal(found.Unanswerable, unanswerable) || found.Held != nil || found.Refused != nil {
				t.Fatalf("started %v, standing %+v", c.started, found)
			}
		})
	}
	cases := map[string]func(*cluster){
		"inputs that do not parse":        func(c *cluster) { c.inputs = `{"members":` },
		"a Canary of another Application": func(c *cluster) { c.canaries["auth-ui"] = gate.Canary{Application: "mail", Revision: revision} },
		"a revision that is no revision": func(c *cluster) {
			c.canaries = map[string]gate.Canary{"auth-api": {Application: "auth", Revision: "x"}, "auth-ui": {Application: "auth", Revision: "x"}}
		},
		// The identity is the Application's own: inputs naming another would start another Job.
		"inputs naming another migration identity": func(c *cluster) {
			*c = *releasing()
			c.inputs = strings.Replace(migratingInputs, `"identity":"auth-migration"`, `"identity":"mail-migration"`, 1)
			c.jobs = append(c.jobs, gate.Job{Name: "mail-migration-9d2c4e6a8b0d", Component: "mail-migration", Suspended: true})
		},
		"a migration start the cluster fails": func(c *cluster) { *c = *releasing(); c.away = "start" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			c := failedRelease()
			edit(c)

			found := tend(t, c)

			if c.started != nil || !slices.Equal(found.Unanswerable, unanswerable) {
				t.Fatalf("started %v, standing %+v", c.started, found)
			}
		})
	}
	t.Run("the list of Applications away", func(t *testing.T) {
		c := failedRelease()
		c.away = "applications"
		g := gate.New(c)
		if err := g.Tend(t.Context(), now, quiet); err == nil || !g.Standing().At.IsZero() {
			t.Fatalf("tend = %v, standing %+v", err, g.Standing())
		}
	})
}

func TestInputsUnderOneIdInAnotherNamespaceSilenceNothing(t *testing.T) {
	c := releasing()
	c.also = []gate.Gated{{Namespace: "elsewhere", Application: "auth"}}

	found := tend(t, c)

	// auth's own migration still starts; the inputs elsewhere are tended as elsewhere's.
	if !slices.Contains(c.started, upJob) || slices.Contains(found.Unanswerable, app) {
		t.Fatalf("started %v, standing %+v", c.started, found)
	}
}

func TestNothingIsConcludedWhileARenderIsBeingApplied(t *testing.T) {
	c := failedRelease()
	// Flux has applied the next render's Canary for the UI, and not yet the API's.
	c.canaries["auth-ui"] = gate.Canary{Application: "auth", Revision: "sha256:1111111111111111", Phase: "Succeeded", Applied: "u1", Promoted: "u1"}

	found := tend(t, c)

	if c.started != nil || c.written != nil || found.Held != nil || found.Refused != nil || found.Unanswerable != nil {
		t.Fatalf("started %v, wrote %+v, standing %+v", c.started, c.written, found)
	}
}

func TestAMigrationTheInputsNameIsAStepBeforeItsJobIsApplied(t *testing.T) {
	c := auth()
	c.inputs = migratingInputs

	got, err := gate.New(c).MayStart(t.Context(), asks())

	if err != nil || got.Yes || got.Why != "auth-migration-9d2c4e6a8b0d is not there yet" {
		t.Fatalf("may-start = %+v, %v", got, err)
	}
}

// heard is a log the test may read while the gate still writes it.
type heard struct {
	mu   sync.Mutex
	text strings.Builder
}

func (h *heard) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.text.Write(p)
}

func (h *heard) said(what string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Contains(h.text.String(), what)
}

func TestTheGateTendsOnEveryTickUntilItIsStopped(t *testing.T) {
	run := func(c *cluster, logger *slog.Logger, until func(*gate.Gate) bool) *gate.Gate {
		g := gate.New(c)
		ctx, stop := context.WithCancel(t.Context())
		ticks := make(chan time.Time)
		done := make(chan struct{})
		go func() { g.Run(ctx, ticks, logger); close(done) }()
		ticks <- now
		ticks <- now.Add(time.Minute)
		for !until(g) {
			runtime.Gosched()
		}
		stop()
		<-done
		return g
	}

	c := releasing()
	run(c, quiet, func(g *gate.Gate) bool { return g.Standing().At.Equal(now.Add(time.Minute)) })
	if !slices.Equal(c.started, []string{upJob, upJob}) {
		t.Fatalf("started %v", c.started)
	}

	// A round that cannot list the Applications is said, and the gate keeps what it last found.
	away := releasing()
	away.away = "applications"
	log := &heard{}
	g := run(away, slog.New(slog.NewTextHandler(log, nil)), func(*gate.Gate) bool {
		return log.said("release gate could not tend")
	})
	if !g.Standing().At.IsZero() || away.started != nil {
		t.Fatalf("standing %+v, started %v", g.Standing(), away.started)
	}
}
