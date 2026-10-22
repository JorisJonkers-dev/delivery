package reporter_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/githubapp"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp/githubtest"
	"github.com/JorisJonkers-dev/delivery/internal/loop"
	"github.com/JorisJonkers-dev/delivery/internal/reporter"
)

// cluster is a cluster a test moves by hand.
type cluster struct {
	mu    sync.Mutex
	units []reporter.Unit
	err   error
}

func (c *cluster) set(units ...reporter.Unit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.units = units
}

func (c *cluster) Units(context.Context) ([]reporter.Unit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]reporter.Unit(nil), c.units...), c.err
}

const logURL = "https://github.com/JorisJonkers-dev/estate/actions/runs/38608377925"

// deploy creates the Rollout's Deployment as the composition workflow does.
func deploy(github *githubtest.Server, u reporter.Unit) *githubtest.Deployment {
	return github.Deploy(githubtest.Deployment{
		Repository: u.Rollout.Repository, SHA: u.Rollout.SHA, Environment: u.Rollout.Environment,
		Project: u.Project, Version: u.Rollout.Version, Created: began,
	}, githubtest.Status{State: "in_progress", Description: u.Project + " " + u.Rollout.Version + " pinned", LogURL: logURL})
}

func newReporter(t *testing.T, github *githubtest.Server, c reporter.Cluster) *reporter.Reporter {
	t.Helper()
	key, err := githubapp.ParseKey(github.PEM())
	if err != nil {
		t.Fatal(err)
	}
	return &reporter.Reporter{
		Cluster: c,
		GitHub: githubapp.Deployments{App: githubapp.App{
			API: github.URL, AppID: githubtest.AppID, InstallationID: githubtest.InstallationID, Key: key,
			HTTP: github.Client(), Now: time.Now,
		}},
		Now: time.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// states is each status's state and description, oldest first.
func states(statuses []githubtest.Status) []string {
	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, s.State+": "+s.Description)
	}
	return out
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statuses:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// running starts a Reporter's loop on prompts alone, and returns a channel each pass reports on
// and the Reporter's routes, served.
func running(t *testing.T, r *reporter.Reporter, interval time.Duration) (<-chan bool, *httptest.Server) {
	t.Helper()
	passed := make(chan bool)
	pass := func(ctx context.Context) (bool, error) {
		wrote, err := r.Pass(ctx)
		select {
		case passed <- wrote:
		case <-ctx.Done():
		}
		return wrote, err
	}
	prompts := reporter.NewPrompts()
	passes := loop.New(pass, interval, time.Now, prompts.Every, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		passes.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() { stop(); <-done })
	mux := http.NewServeMux()
	prompts.Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return passed, server
}

// event posts what Flux's generic Provider posts.
func event(t *testing.T, server *httptest.Server, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/flux", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("the event was answered %s", res.Status)
	}
}

const readyEvent = `{"involvedObject":{"kind":"Kustomization","namespace":"flux-system","name":"apps-notes"},
"severity":"info","reason":"ReconciliationSucceeded","message":"Reconciliation finished",
"metadata":{"repository":"JorisJonkers-dev/notes","version":"1.4.2"}}`

func TestEachEventMovesTheRolloutOnAStage(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	under := func(iteration int) reporter.Member {
		return reporter.Member{Process: "notes-api", Phase: "Progressing", Iteration: iteration, Iterations: 3, Transitioned: began.Add(time.Minute)}
	}
	c.set(unit(earlier, reporter.Member{Process: "notes-api", Phase: "Succeeded", Applied: "h1", Promoted: "h1", Serves: true}))
	d := deploy(github, c.units[0])
	// An hour between ticks: only an event starts a pass here.
	passed, server := running(t, newReporter(t, github, c), time.Hour)

	if <-passed {
		t.Fatal("a release Flux has not applied was reported past pinned")
	}
	steps := []reporter.Unit{
		unit(pinned, reporter.Member{Process: "notes-api", Phase: "Succeeded", Applied: "h1", Promoted: "h1", Serves: false}),
		unit(pinned, under(1)),
		unit(pinned, under(3)),
		unit(pinned, reporter.Member{Process: "notes-api", Phase: "WaitingPromotion"}),
		unit(pinned, reporter.Member{Process: "notes-api", Phase: "Promoting"}),
		unit(pinned, promoted("notes-api")),
	}
	for _, step := range steps {
		c.set(step)
		event(t, server, readyEvent)
		if !<-passed {
			t.Fatalf("a pass after %+v wrote nothing", step)
		}
	}
	// A late event about a promoted release writes nothing more.
	event(t, server, readyEvent)
	if <-passed {
		t.Fatal("a promoted release was written to again")
	}

	equal(t, states(github.Statuses(d.ID)), []string{
		"in_progress: notes 1.4.2 pinned",
		"in_progress: applied by Flux; notes-api waits for Flagger",
		"in_progress: applied by Flux; notes-api checks 1/3",
		"in_progress: applied by Flux; notes-api checks 3/3",
		"in_progress: applied by Flux; notes-api waits for the other members",
		"in_progress: applied by Flux; notes-api promoting",
		"success: notes 1.4.2 promoted",
	})
	for _, s := range github.Statuses(d.ID)[1:] {
		if s.LogURL != logURL {
			t.Fatalf("a status lost the run that delivered the release: %+v", s)
		}
	}
}

func TestAnEventCarriesNothingTheReporterBelieves(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	c.set(unit(earlier))
	d := deploy(github, c.units[0])
	passed, server := running(t, newReporter(t, github, c), time.Hour)
	<-passed

	// A forged event claims the release is ready; the cluster says it is not applied.
	event(t, server, `{"involvedObject":{"kind":"Kustomization","namespace":"flux-system","name":"apps-notes"},"reason":"ReconciliationSucceeded"}`)
	if <-passed {
		t.Fatal("a forged event wrote a status")
	}
	event(t, server, "not even JSON")
	if <-passed {
		t.Fatal("a body that is not an event wrote a status")
	}
	equal(t, states(github.Statuses(d.ID)), []string{"in_progress: notes 1.4.2 pinned"})
}

func TestARolloutWhoseEventsWereDroppedReachesItsFinalStatusOnTheNextPass(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	c.set(unit(earlier))
	d := deploy(github, c.units[0])
	passed, _ := running(t, newReporter(t, github, c), 10*time.Millisecond)
	<-passed

	// Flux applied it and Flagger promoted it, and no event arrived.
	c.set(unit(pinned, promoted("notes-api")))
	for wrote := false; !wrote; {
		wrote = <-passed
	}
	equal(t, states(github.Statuses(d.ID)), []string{"in_progress: notes 1.4.2 pinned", "success: notes 1.4.2 promoted"})
}

func TestARolledBackCanaryEndsItsRolloutInFailure(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	u := unit(pinned, reporter.Member{Process: "notes-api", Phase: "Progressing", Iteration: 2, Iterations: 3, Transitioned: began.Add(time.Minute)})
	c.set(u)
	d := deploy(github, u)
	r := newReporter(t, github, c)
	ctx := t.Context()
	if _, err := r.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	c.set(unit(pinned, reporter.Member{Process: "notes-api", Phase: "Failed", Transitioned: began.Add(5 * time.Minute), Message: "Canary analysis failed, Deployment scaled to zero."}))
	if _, err := r.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	equal(t, states(github.Statuses(d.ID)), []string{
		"in_progress: notes 1.4.2 pinned",
		"in_progress: applied by Flux; notes-api checks 2/3",
		"failure: notes-api rolled back: Canary analysis failed, Deployment scaled to zero.",
	})
}

func TestAFluxFailureMayBeFollowedBySuccess(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	failing := unit(earlier)
	failing.Attempted = pinned
	failing.Ready = reporter.Condition{Status: "False", Reason: "HealthCheckFailed", Message: "timeout"}
	c.set(failing)
	d := deploy(github, failing)
	r := newReporter(t, github, c)
	ctx := t.Context()
	for _, u := range []reporter.Unit{failing, failing, unit(pinned)} {
		c.set(u)
		if _, err := r.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	equal(t, states(github.Statuses(d.ID)), []string{
		"in_progress: notes 1.4.2 pinned",
		"failure: Flux could not apply notes 1.4.2: HealthCheckFailed: timeout",
		"success: notes 1.4.2 applied by Flux and ready",
	})
}

func TestAReporterThatRestartsWritesNoStatusTwice(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	u := unit(pinned, reporter.Member{Process: "notes-api", Phase: "Progressing", Iteration: 1, Iterations: 3, Transitioned: began.Add(time.Minute)})
	c.set(u)
	d := deploy(github, u)
	ctx := t.Context()
	for range 2 {
		// A Reporter that starts again knows nothing but what GitHub holds.
		if _, err := newReporter(t, github, c).Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	equal(t, states(github.Statuses(d.ID)), []string{
		"in_progress: notes 1.4.2 pinned",
		"in_progress: applied by Flux; notes-api checks 1/3",
	})
}

func TestTheReporterWritesOnlyToTheDeploymentOfTheRelease(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	u := unit(pinned)
	u.Rollout.URL = "https://notes.jorisjonkers.dev"
	// A Deployment of the same commit, for another Project released from the same repository.
	other := github.Deploy(githubtest.Deployment{
		Repository: u.Rollout.Repository, SHA: u.Rollout.SHA, Environment: "production", Project: "notes-admin", Version: "1.4.2", Created: began,
	}, githubtest.Status{State: "in_progress", Description: "notes-admin 1.4.2 pinned"})
	c.set(u)
	r := newReporter(t, github, c)
	ctx := t.Context()
	if wrote, err := r.Pass(ctx); err != nil || wrote {
		t.Fatalf("wrote %v, %v to a Deployment of another Project", wrote, err)
	}
	equal(t, states(github.Statuses(other.ID)), []string{"in_progress: notes-admin 1.4.2 pinned"})

	// The workflow creates it later; the Reporter asks again only once its memory of the absence
	// is old.
	d := deploy(github, u)
	if wrote, _ := r.Pass(ctx); wrote {
		t.Fatal("asked again within the absence's memory")
	}
	r.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if wrote, err := r.Pass(ctx); err != nil || !wrote {
		t.Fatalf("wrote %v, %v once the Deployment exists", wrote, err)
	}
	if got := github.Statuses(d.ID); got[len(got)-1].EnvironmentURL != "https://notes.jorisjonkers.dev" {
		t.Fatalf("the status names no environment URL: %+v", got)
	}
}

func TestASupersededRolloutIsLeftAlone(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	u := unit(pinned)
	d := deploy(github, u)
	github.Deploy(githubtest.Deployment{Repository: "JorisJonkers-dev/other", SHA: strings.Repeat("b", 40), Environment: "production", Project: "other", Version: "1"},
		githubtest.Status{State: "in_progress"})
	// What the composition workflow writes to the Deployment a later release supersedes.
	r := newReporter(t, github, c)
	ctx := t.Context()
	key, _ := githubapp.ParseKey(github.PEM())
	app := githubapp.Deployments{App: githubapp.App{API: github.URL, AppID: githubtest.AppID, InstallationID: githubtest.InstallationID, Key: key, HTTP: github.Client(), Now: time.Now}}
	if err := app.Post(ctx, u.Rollout.Repository, d.ID, githubapp.Status{State: "inactive", Description: "superseded by 1.4.3"}); err != nil {
		t.Fatal(err)
	}
	c.set(u)
	if wrote, err := r.Pass(ctx); err != nil || wrote {
		t.Fatalf("wrote %v, %v to a superseded Rollout", wrote, err)
	}
}

func TestUnitsWithoutARolloutOrWithOneThatIsNotAreSkipped(t *testing.T) {
	github := githubtest.New(t, "")
	estate := reporter.Unit{Name: "estate-vso-secrets", Project: "estate-vso-secrets"}
	forged := unit(pinned)
	forged.Name = "apps-forged"
	forged.Rollout.Repository = "JorisJonkers-dev/notes/../../orgs/x"
	short := unit(pinned)
	short.Rollout.SHA = "33a6430"
	environment := unit(pinned)
	environment.Rollout.Environment = "prod uction"
	unversioned := unit(pinned)
	unversioned.Rollout.Version = ""
	c := &cluster{}
	c.set(estate, forged, short, environment, unversioned)
	if wrote, err := newReporter(t, github, c).Pass(t.Context()); err != nil || wrote {
		t.Fatalf("wrote %v, %v", wrote, err)
	}
}

func TestAPassThatCannotReadOrWriteSaysSo(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{err: errors.New("the API server is away")}
	r := newReporter(t, github, c)
	if _, err := r.Pass(t.Context()); err == nil || !strings.Contains(err.Error(), "the API server is away") {
		t.Fatalf("err = %v", err)
	}

	// A Deployment id GitHub does not hold, for a release it does list: the status cannot be read.
	c = &cluster{}
	u := unit(pinned)
	c.set(u)
	r = newReporter(t, github, c)
	r.GitHub = lying{r.GitHub}
	if _, err := r.Pass(t.Context()); err == nil || !strings.Contains(err.Error(), "apps-notes") {
		t.Fatalf("err = %v", err)
	}

	// An App GitHub does not know cannot find anything.
	r = newReporter(t, github, c)
	deployments, _ := r.GitHub.(githubapp.Deployments)
	deployments.AppID = "1"
	r.GitHub = deployments
	if _, err := r.Pass(t.Context()); err == nil || !strings.Contains(err.Error(), "sign in as the App") {
		t.Fatalf("err = %v", err)
	}
}

// lying finds a Deployment GitHub does not hold.
type lying struct{ reporter.GitHub }

func (lying) Find(context.Context, string, string, string) (githubapp.Deployment, bool, error) {
	return githubapp.Deployment{ID: 404, Project: "notes", Version: "1.4.2", Created: began}, true, nil
}

// A release pinned again at the same version and commit, as a toolkit change moves every pin,
// is a new Rollout on a new Deployment: the one before it being done says nothing of it.
func TestARolloutPinnedAgainAtTheSameReleaseIsReportedOnItsOwnDeployment(t *testing.T) {
	github := githubtest.New(t, "")
	c := &cluster{}
	first := unit(pinned)
	before := deploy(github, first)
	c.set(first)
	r := newReporter(t, github, c)
	ctx := t.Context()
	if wrote, err := r.Pass(ctx); err != nil || !wrote {
		t.Fatalf("wrote %v, %v for the first pin", wrote, err)
	}

	again := unit(earlier)
	again.Pinned = earlier
	after := deploy(github, again)
	c.set(again)
	if wrote, err := r.Pass(ctx); err != nil || !wrote {
		t.Fatalf("wrote %v, %v for the release pinned again", wrote, err)
	}
	equal(t, states(github.Statuses(after.ID)), []string{
		"in_progress: notes 1.4.2 pinned",
		"success: notes 1.4.2 applied by Flux and ready",
	})
	if got := states(github.Statuses(before.ID)); strings.Count(strings.Join(got, "\n"), "success: ") != 1 {
		t.Fatalf("the first pin's Deployment was reported again: %v", got)
	}
}
