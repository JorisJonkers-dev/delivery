package gate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

const (
	revision = "sha256:9d2c4e6a8b0d1f3a5c7e9b1d3f5a7c9e1b3d5f7a9c1e3b5d7f9a1c3e5b7d9f1a"
	earlier  = "sha256:0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"
	// auth's release-gate inputs, as the render writes them: one line of canonical JSON.
	authInputs = `{"analysis":{"interval":"30s","iterations":4,"threshold":3},"deadline":"1800s","endpoint":"http://release-gate.delivery-system.svc.cluster.local:8080","members":[{"checks":["error-rate","latency"],"process":"auth-api","readiness":{"path":"/api/actuator/health/readiness","port":8081}},{"process":"auth-ui","readiness":{"path":"/","port":8080}}]}`
)

// cluster is an in-memory gate.Cluster: what the gate would read, set by each test.
type cluster struct {
	inputs   string
	jobs     []gate.Job
	canaries map[string]gate.Canary
	pods     map[string][]gate.Pod
	// changed names the members whose Deployment no longer holds what their primary runs.
	changed map[string]bool
	// away names the read that fails, as an unreachable API server would fail it.
	away string
}

var errAway = errors.New("the API server is away")

func (c *cluster) Inputs(context.Context, string, string) (string, error) {
	if c.away == "inputs" {
		return "", errAway
	}
	return c.inputs, nil
}

func (c *cluster) Jobs(context.Context, string, string) ([]gate.Job, error) {
	if c.away == "jobs" {
		return nil, errAway
	}
	return c.jobs, nil
}

func (c *cluster) Canary(_ context.Context, _, process string) (gate.Canary, error) {
	if c.away == "canary" {
		return gate.Canary{}, errAway
	}
	canary, held := c.canaries[process]
	if !held {
		return gate.Canary{}, gate.ErrNoCanary
	}
	return canary, nil
}

func (c *cluster) NewCopy(_ context.Context, _, process string) ([]gate.Pod, error) {
	if c.away == "pods" {
		return nil, errAway
	}
	return c.pods[process], nil
}

func (c *cluster) Serves(_ context.Context, _, process string) (bool, error) {
	if c.away == "serves" {
		return false, errAway
	}
	return !c.changed[process], nil
}

// of is a Canary of auth at the revision under release.
func of(phase, applied, promoted string, iterations int) gate.Canary {
	return gate.Canary{Application: "auth", Revision: revision, Phase: phase, Applied: applied, Promoted: promoted, Iterations: iterations}
}

// auth is a release of auth under way: the API has run its four iterations, the UI one.
func auth() *cluster {
	return &cluster{
		inputs: authInputs,
		canaries: map[string]gate.Canary{
			"auth-api": of("Progressing", "a2", "a1", 4),
			"auth-ui":  of("Progressing", "u2", "u1", 1),
		},
		changed: map[string]bool{},
		pods: map[string][]gate.Pod{
			"auth-api": {{Name: "auth-api-7c9d-x", Ready: true}},
			"auth-ui":  {{Name: "auth-ui-5b8f-y", Ready: true}},
		},
	}
}

// asks is the question the API's Canary puts, at the revision under release.
func asks() gate.Question { return asksAs("auth-api") }

func asksAs(process string) gate.Question {
	return gate.Question{Namespace: "auth-system", Application: "auth", Process: process, Revision: revision}
}

// midRelease is a gate over c that the UI has already asked a question of at this revision, as
// Flagger does on every tick of its analysis.
func midRelease(t *testing.T, c *cluster) *gate.Gate {
	t.Helper()
	g := gate.New(c)
	if _, err := g.Checks(t.Context(), asksAs("auth-ui")); err != nil {
		t.Fatal(err)
	}
	return g
}

type question func(*gate.Gate, context.Context, gate.Question) (gate.Answer, error)

func TestAReleaseStartsOnceItsMigrationAndEveryPrepareProcessCompleted(t *testing.T) {
	migration := gate.Job{Name: "auth-migration-9d2c4e6a8b0d", Component: "auth-migration"}
	prepare := gate.Job{Name: "auth-seed-9d2c4e6a8b0d", Component: "auth-seed"}
	done := func(j gate.Job) gate.Job { j.Complete = true; return j }
	failed := func(j gate.Job) gate.Job { j.Failed = true; return j }

	cases := map[string]struct {
		jobs []gate.Job
		yes  bool
		why  string
	}{
		"no migration and no prepare Process": {nil, true, "the release has no migration and no prepare Process"},
		"both completed":                      {[]gate.Job{done(migration), done(prepare)}, true, "the migration and every prepare Process completed"},
		"the migration still suspended":       {[]gate.Job{migration, done(prepare)}, false, "auth-migration-9d2c4e6a8b0d has not completed"},
		"a prepare Process still running":     {[]gate.Job{done(migration), prepare}, false, "auth-seed-9d2c4e6a8b0d has not completed"},
		"the migration failed":                {[]gate.Job{failed(migration)}, false, "auth-migration-9d2c4e6a8b0d failed: the release is held"},
		// The Job that undoes a migration is rendered suspended beside it and stays so in a release that goes well.
		"the down Job, never started": {
			[]gate.Job{done(migration), {Name: "auth-migration-down-9d2c4e6a8b0d", Component: "auth-migration-down"}},
			true, "the migration and every prepare Process completed",
		},
		// An Application that has migrated before migrates in every release: a Job of this revision
		// that is not applied yet is a step not done, not a step the release lacks.
		"an earlier revision's migration, and none of this one yet": {
			[]gate.Job{done(gate.Job{Name: "auth-migration-0a1b2c3d4e5f", Component: "auth-migration"})},
			false, "auth-migration-9d2c4e6a8b0d is not there yet",
		},
		"an earlier revision's failed migration beside this one's": {
			[]gate.Job{failed(gate.Job{Name: "auth-migration-0a1b2c3d4e5f", Component: "auth-migration"}), done(migration)},
			true, "the migration and every prepare Process completed",
		},
		// A Job is this revision's only under its identity's own name: a look-alike is not it.
		"a Job that only ends like this revision's": {
			[]gate.Job{done(gate.Job{Name: "other-auth-migration-9d2c4e6a8b0d", Component: "auth-migration"})},
			false, "auth-migration-9d2c4e6a8b0d is not there yet",
		},
		"a Job with no identity, and an earlier down Job": {
			[]gate.Job{{Name: "stray-9d2c4e6a8b0d"}, {Name: "auth-migration-down-0a1b2c3d4e5f", Component: "auth-migration-down"}},
			true, "the release has no migration and no prepare Process",
		},
		// Steps are checked in name order, so what the answer names does not depend on list order.
		"two steps undone, listed backwards": {
			[]gate.Job{prepare, migration}, false, "auth-migration-9d2c4e6a8b0d has not completed",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := auth()
			c.jobs = tc.jobs
			got, err := gate.New(c).MayStart(t.Context(), asks())
			if err != nil || got.Yes != tc.yes || got.Why != tc.why {
				t.Fatalf("may-start = %+v, %v", got, err)
			}
		})
	}
}

func TestAnIterationPassesWhenEveryPodOfTheNewCopyIsReadyAndNoneRestarted(t *testing.T) {
	cases := map[string]struct {
		pods []gate.Pod
		yes  bool
		why  string
	}{
		"ready":            {[]gate.Pod{{Name: "a", Ready: true}, {Name: "b", Ready: true}}, true, "every pod of the new copy is ready and none restarted"},
		"one not ready":    {[]gate.Pod{{Name: "a", Ready: true}, {Name: "b"}}, false, "b is not ready"},
		"one restarted":    {[]gate.Pod{{Name: "a", Ready: true, Restarts: 1}}, false, "a restarted"},
		"no pod at all":    {nil, false, "the new copy of auth-api has no pod"},
		"restarted, ready": {[]gate.Pod{{Name: "a", Ready: true}, {Name: "b", Ready: true, Restarts: 2}}, false, "b restarted"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := auth()
			c.pods["auth-api"] = tc.pods
			got, err := gate.New(c).Checks(t.Context(), asks())
			if err != nil || got.Yes != tc.yes || got.Why != tc.why {
				t.Fatalf("checks = %+v, %v", got, err)
			}
		})
	}
}

func TestNoMemberIsPromotedUntilEveryMemberHasPassed(t *testing.T) {
	ui := func(c gate.Canary) *cluster {
		cl := auth()
		cl.canaries["auth-ui"] = c
		return cl
	}
	cases := map[string]struct {
		cluster *cluster
		yes     bool
		why     string
	}{
		"the other member is still analysed": {auth(), false, "auth-ui has not passed its analysis (Progressing)"},
		// Flagger's record says it serves what it last saw, and its Deployment already holds more.
		"the other member changed a moment ago": {
			func() *cluster {
				c := ui(of("Succeeded", "u1", "u1", 0))
				c.changed["auth-ui"] = true
				return c
			}(), false, "auth-ui changed and has not started its analysis",
		},
		"the other member waits at the barrier": {
			ui(of("WaitingPromotion", "u2", "u1", 0)), true, "every member passed its analysis or did not change",
		},
		"the other member is being promoted": {
			ui(of("Promoting", "u2", "u1", 0)), true, "every member passed its analysis or did not change",
		},
		"the other member is finishing its promotion": {
			ui(of("Finalising", "u2", "u1", 0)), true, "every member passed its analysis or did not change",
		},
		// Promoted seconds ago, or unchanged by this revision: its primary serves what the render holds.
		"the other member's primary serves the render": {
			ui(of("Succeeded", "u1", "u1", 0)), true, "every member passed its analysis or did not change",
		},
		"the other member was only ever initialised": {
			ui(of("Initialized", "u1", "u1", 0)), true, "every member passed its analysis or did not change",
		},
		// Changed by this revision, and Flagger has not started on it: what it serves is the old version.
		"the other member changed and has not started": {
			ui(of("Succeeded", "u2", "u1", 0)), false, "auth-ui has not passed its analysis (Succeeded)",
		},
		"the other member never recorded what it serves": {
			ui(of("Succeeded", "", "", 0)), false, "auth-ui has not passed its analysis (Succeeded)",
		},
		"the other member failed": {
			ui(of("Failed", "u2", "u1", 0)), false, "auth-ui has not passed its analysis (Failed)",
		},
		"the other member has no phase yet": {
			ui(of("", "", "", 0)), false, "auth-ui has not passed its analysis ()",
		},
		// Its Canary is still the earlier render's: nobody has asked it about this revision.
		"the other member is of an earlier revision": {
			ui(gate.Canary{Application: "auth", Revision: earlier, Phase: "Succeeded", Applied: "u1", Promoted: "u1"}), false, "auth-ui is not at this revision",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := midRelease(t, tc.cluster).MayPromote(t.Context(), asks())
			if err != nil || got.Yes != tc.yes || got.Why != tc.why {
				t.Fatalf("may-promote = %+v, %v", got, err)
			}
		})
	}
}

func TestAMemberSeenWaitingCountsOnlyOnceItHasAskedAtThisRevision(t *testing.T) {
	// Flagger's status carries no revision. A member read as waiting, promoting or finishing may
	// be so from the release before, a moment before Flagger notices this one.
	for _, phase := range []string{"WaitingPromotion", "Promoting", "Finalising"} {
		c := auth()
		c.canaries["auth-ui"] = of(phase, "u2", "u1", 4)
		g := gate.New(c)

		got, err := g.MayPromote(t.Context(), asks())
		if err != nil || got.Yes || got.Why != "auth-ui has not asked at this revision yet" {
			t.Fatalf("%s, never asked = %+v, %v", phase, got, err)
		}
		// It asks, at this revision, whatever the answer: Flagger is switching it at this one.
		if _, err := g.MayStart(t.Context(), asksAs("auth-ui")); err != nil {
			t.Fatal(err)
		}
		if got, err := g.MayPromote(t.Context(), asks()); err != nil || !got.Yes {
			t.Fatalf("%s, once it asked = %+v, %v", phase, got, err)
		}
		// A gate that restarts remembers nothing, and so withholds the yes until it asks again.
		if got, err := gate.New(c).MayPromote(t.Context(), asks()); err != nil || got.Yes {
			t.Fatalf("%s, after a restart = %+v, %v", phase, got, err)
		}
	}

	// A question at another revision than its Canary's is no evidence of this one.
	c := auth()
	c.canaries["auth-ui"] = of("WaitingPromotion", "u2", "u1", 4)
	g := gate.New(c)
	stale := asksAs("auth-ui")
	stale.Revision = earlier
	if _, err := g.Checks(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	if got, err := g.MayPromote(t.Context(), asks()); err != nil || got.Yes {
		t.Fatalf("after a question at an earlier revision = %+v, %v", got, err)
	}
}

func TestTheMemberThatAsksIsHeldToItsOwnRecord(t *testing.T) {
	cases := map[string]struct {
		asker gate.Canary
		yes   bool
		why   string
	}{
		// Flagger marks a member as waiting only after its first refusal: the first time it asks
		// it is still being analysed, with every iteration counted.
		"analysed, every iteration counted": {of("Progressing", "a2", "a1", 4), true, "every member passed its analysis or did not change"},
		"more iterations than asked for":    {of("Progressing", "a2", "a1", 5), true, "every member passed its analysis or did not change"},
		"waiting at the barrier":            {of("WaitingPromotion", "a2", "a1", 0), true, "every member passed its analysis or did not change"},
		"analysed, an iteration short":      {of("Progressing", "a2", "a1", 3), false, "auth-api has not passed its analysis (Progressing, 3 of 4)"},
		"failed":                            {of("Failed", "a2", "a1", 4), false, "auth-api has not passed its analysis (Failed, 4 of 4)"},
		"not being analysed at all":         {of("Succeeded", "a1", "a1", 4), false, "auth-api has not passed its analysis (Succeeded, 4 of 4)"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := auth()
			c.canaries["auth-ui"] = of("WaitingPromotion", "u2", "u1", 4)
			c.canaries["auth-api"] = tc.asker
			got, err := midRelease(t, c).MayPromote(t.Context(), asks())
			if err != nil || got.Yes != tc.yes || got.Why != tc.why {
				t.Fatalf("may-promote = %+v, %v", got, err)
			}
		})
	}
}

func TestAQuestionIsAnsweredOnlyAtTheRevisionItsCanaryCarries(t *testing.T) {
	// The Canary is of another render than the question: one that was replaced, or one a caller
	// who cannot read the Canary made up. Nothing is read past it, and nothing is said of it.
	for name, ask := range map[string]question{"may-start": (*gate.Gate).MayStart, "checks": (*gate.Gate).Checks, "may-promote": (*gate.Gate).MayPromote} {
		c := auth()
		c.canaries["auth-api"] = gate.Canary{Application: "auth", Revision: earlier, Phase: "WaitingPromotion", Iterations: 4}
		c.canaries["auth-ui"] = of("WaitingPromotion", "u2", "u1", 4)
		// Reading on would fail: the answer comes before any of it, the inputs included.
		c.away = "inputs"
		c.pods = nil
		got, err := ask(gate.New(c), t.Context(), asks())
		if err != nil || got.Yes || got.Why != "auth-api is not at this revision" {
			t.Fatalf("%s = %+v, %v", name, got, err)
		}
		// A Canary that is not there reads the same: whether anything is deployed is not said.
		delete(c.canaries, "auth-api")
		absent, err := ask(gate.New(c), t.Context(), asks())
		if err != nil || absent != got {
			t.Fatalf("%s with no Canary = %+v, %v, want %+v", name, absent, err, got)
		}
	}
}

func TestACanaryOfAnotherApplicationIsNotTheQuestions(t *testing.T) {
	for name, ask := range map[string]question{"may-start": (*gate.Gate).MayStart, "checks": (*gate.Gate).Checks, "may-promote": (*gate.Gate).MayPromote} {
		c := auth()
		c.canaries["auth-api"] = gate.Canary{Application: "mail", Revision: revision, Phase: "WaitingPromotion"}
		if got, err := ask(gate.New(c), t.Context(), asks()); err == nil || got.Yes {
			t.Fatalf("%s for a Canary of another Application = %+v, %v", name, got, err)
		}
	}
	// A member whose Canary cannot be read holds the barrier shut.
	gone := auth()
	delete(gone.canaries, "auth-ui")
	if got, err := gate.New(gone).MayPromote(t.Context(), asks()); err == nil || got.Yes {
		t.Fatalf("may-promote past a member with no Canary = %+v, %v", got, err)
	}
	// And a member's Canary that names another Application holds no place at this barrier.
	c := auth()
	c.canaries["auth-ui"] = gate.Canary{Application: "mail", Revision: revision, Phase: "WaitingPromotion"}
	if got, err := gate.New(c).MayPromote(t.Context(), asks()); err == nil || got.Yes {
		t.Fatalf("may-promote past a member of another Application = %+v, %v", got, err)
	}
}

func TestTheGateFailsClosed(t *testing.T) {
	questions := map[string]question{"may-start": (*gate.Gate).MayStart, "checks": (*gate.Gate).Checks, "may-promote": (*gate.Gate).MayPromote}
	breaks := map[string]func(*cluster){
		"the inputs cannot be read":         func(c *cluster) { c.away = "inputs" },
		"the inputs are not JSON":           func(c *cluster) { c.inputs = "{" },
		"the inputs are not a release gate": func(c *cluster) { c.inputs = `{"members":[]}` },
		"the Process is no member of the gate": func(c *cluster) {
			c.inputs = `{"analysis":{"interval":"30s","iterations":4,"threshold":3},"deadline":"60s","endpoint":"http://gate","members":[{"process":"someone-else","readiness":{"tcp":5432}}]}`
		},
	}
	for name, ask := range questions {
		for what, breakIt := range breaks {
			t.Run(name+" when "+what, func(t *testing.T) {
				c := auth()
				breakIt(c)
				if got, err := ask(gate.New(c), t.Context(), asks()); err == nil || got.Yes {
					t.Fatalf("%s = %+v, %v: want an error and no yes", name, got, err)
				}
			})
		}
	}

	for name, tc := range map[string]struct {
		ask   question
		away  string
		tweak func(*gate.Question)
	}{
		"may-start when the Jobs cannot be read":      {(*gate.Gate).MayStart, "jobs", nil},
		"may-start at something that is no revision":  {(*gate.Gate).MayStart, "", func(q *gate.Question) { q.Revision = "latest" }},
		"may-start at a revision too short to name":   {(*gate.Gate).MayStart, "", func(q *gate.Question) { q.Revision = "sha256:9d2c" }},
		"checks when the pods cannot be read":         {(*gate.Gate).Checks, "pods", nil},
		"may-promote when a Canary cannot be read":    {(*gate.Gate).MayPromote, "canary", nil},
		"may-start when its Canary cannot be read":    {(*gate.Gate).MayStart, "canary", nil},
		"checks when its Canary cannot be read":       {(*gate.Gate).Checks, "canary", nil},
		"may-promote when a Deployment is unreadable": {(*gate.Gate).MayPromote, "serves", nil},
	} {
		t.Run(name, func(t *testing.T) {
			c := auth()
			// The other member looks unchanged, so the barrier goes on to compare its Deployments.
			c.canaries["auth-ui"] = of("Succeeded", "u1", "u1", 0)
			c.away = tc.away
			q := asks()
			if tc.tweak != nil {
				// The Canary carries the same odd revision, so the question is its own.
				tc.tweak(&q)
				asker := c.canaries["auth-api"]
				asker.Revision = q.Revision
				c.canaries["auth-api"] = asker
			}
			if got, err := tc.ask(gate.New(c), t.Context(), q); err == nil || got.Yes {
				t.Fatalf("%+v, %v: want an error and no yes", got, err)
			}
		})
	}
}

func TestTheInputsAreNamedForTheApplication(t *testing.T) {
	if gate.InputsName("auth") != "auth-release-gate" || gate.InputsKey != "releaseGate.json" {
		t.Fatal("the gate reads the ConfigMap and the key the render writes")
	}
}
