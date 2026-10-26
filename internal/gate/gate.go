// Package gate is the Release Gate's decisions: whether a member's new version may start, whether
// it passes an analysis iteration, and whether it may be promoted
// (deploy-kit spec/v1/55-delivery.md#the-release-gate).
//
// Every answer is read from the cluster at the moment it is asked: the Application's
// release-gate inputs, its release Jobs, its members' Canaries and Deployments, and the pods of
// the new copy. And it fails closed: an answer it cannot read is no.
//
// It answers Flagger and nobody else: a request is Flagger's when it comes from the address of
// one of Flagger's pods (http.go). And it answers a question only at the revision the asking
// member's Canary carries, so a question about a render that has been replaced is refused.
//
// In memory it remembers one thing: which revision each member last asked at. Flagger's status
// carries no revision, so a member "waiting for promotion" could be waiting from the release
// before; a member that has asked at this revision is one Flagger is switching at it. The
// memory only ever withholds a yes: a gate that restarts has none, and the barrier opens again
// as each waiting member asks, which it does on every tick.
//
// Between questions it tends every gated Application on its own clock (tend.go): it records
// what each serves in a ConfigMap of its own, starts a migration whose proof holds, undoes one
// only under every condition of the Down, and says in its log which releases are held.
package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/deploykit/resolved"
)

// InputsKey is the one key of an Application's release-gate ConfigMap: its releaseGate element.
const InputsKey = "releaseGate.json"

// InputsName is the name of an Application's release-gate ConfigMap.
func InputsName(application string) string { return application + "-release-gate" }

// tagLength is how many hex digits of the Application revision name its release Jobs.
const tagLength = 12

// Question is what Flagger asks: one member of one Application, at one Application revision.
type Question struct {
	// Namespace is the Canary's, and so the Application's.
	Namespace   string
	Application string
	// Process is the member the Canary switches.
	Process string
	// Revision is the Application revision the Canary was rendered at.
	Revision string
}

// Answer is the gate's reply. Why says, for a log and for whoever reads the release, what it waits on.
type Answer struct {
	Yes bool
	Why string
}

func yes(why string) Answer { return Answer{Yes: true, Why: why} }

func no(format string, args ...any) Answer { return Answer{Why: fmt.Sprintf(format, args...)} }

// Job is one release Job of an Application: its migration, or a prepare Process.
type Job struct {
	Name string
	// Component is the Job's `app.kubernetes.io/name`: the identity it runs as.
	Component string
	Complete  bool
	Failed    bool
	// Suspended is whether the Job still waits to be started: the render applies it so.
	Suspended bool
	// Version is the version of the Job as read, which starting it is conditional on.
	Version string
}

// Canary is what Flagger records of one member.
type Canary struct {
	// Application is the Application the Canary's webhooks carry: the one its member belongs to.
	Application string
	// Revision is the Application revision the Canary's webhooks carry: the render it was applied from.
	Revision string
	Phase    string
	// Applied and Promoted are Flagger's digests of the member's spec: the one it last saw, and
	// the one its primary serves.
	Applied  string
	Promoted string
	// Iterations is how many analysis iterations Flagger has counted for the member.
	Iterations int
	// Port is the port of the Services Flagger generates for the member, its Canary service's
	// among them.
	Port int
	// Paths and Readiness are what the Canary's webhooks hand the gate to request: the path of
	// every Route to the member, and the path its readiness is asked on.
	Paths     []string
	Readiness string
	// Hostnames are the public hosts the Canary's webhooks hand the gate to warm through, once
	// the Application serves the revision.
	Hostnames []string
	// Transitioned is when Flagger last moved the member to another phase.
	Transitioned time.Time
}

// Pod is one pod of a member's new copy.
type Pod struct {
	Name     string
	Ready    bool
	Restarts int32
}

// Cluster is what the gate reads. Every method reports a thing that is not there as an error:
// the gate does not tell "absent" from "unreadable", because neither is an answer.
type Cluster interface {
	// Flagger returns the addresses of Flagger's own pods: the only callers the gate answers.
	Flagger(ctx context.Context) ([]string, error)
	// Scrapers returns the addresses of the metrics stack's pods: the only callers GET /metrics
	// answers. None where the gate is not told who the metrics stack is.
	Scrapers(ctx context.Context) ([]string, error)
	// Inputs returns the Application's release-gate ConfigMap's one value.
	Inputs(ctx context.Context, namespace, application string) (string, error)
	// Jobs returns the Jobs of the Application: every one labelled as part of it.
	Jobs(ctx context.Context, namespace, application string) ([]Job, error)
	// Canary returns the member's Canary, and ErrNoCanary where there is none.
	Canary(ctx context.Context, namespace, process string) (Canary, error)
	// NewCopy returns the pods of the member's new copy, never its primary's.
	NewCopy(ctx context.Context, namespace, process string) ([]Pod, error)
	// Started reports whether a Process that switches stop-start has started the version its
	// Deployment now holds: the Deployment has observed its current spec, and every replica runs
	// that spec and is available.
	Started(ctx context.Context, namespace, process string) (bool, error)
	// Serves reports whether the member's primary runs what its Deployment now holds: the same
	// pod, but for the names Flagger gives the configuration it copies. It is read off the two
	// Deployments, not off Flagger's record, which is a tick behind a Deployment that just changed.
	Serves(ctx context.Context, namespace, process string) (bool, error)
	// Applications returns every Application that carries release-gate inputs.
	Applications(ctx context.Context) ([]Gated, error)
	// Record returns what the gate recorded of the Application, and ErrNoRecord where it has
	// recorded nothing.
	Record(ctx context.Context, namespace, application string) (Record, error)
	// SetRecord writes the Application's record, which only the gate writes.
	SetRecord(ctx context.Context, namespace, application string, record Record) error
	// Start unsuspends a release Job, as it was read. It is the one field of a rendered object the
	// gate writes.
	Start(ctx context.Context, namespace string, job Job) error
	// SyncObjects returns the Application's Vault sync objects that name the revision that
	// rendered them: those the render keeps past the release that drops them.
	SyncObjects(ctx context.Context, namespace, application string) ([]SyncObject, error)
	// Sweep deletes a sync object as it was read, and nothing that changed since: the one kind of
	// object the gate deletes.
	Sweep(ctx context.Context, namespace string, object SyncObject) error
}

// SyncObject is one VaultAuth, VaultStaticSecret or VaultDynamicSecret of a gated Application,
// and the revision that rendered it
// (deploy-kit spec/v1/55-delivery.md#a-removed-grant-outlives-its-release).
type SyncObject struct {
	Kind     string
	Name     string
	Revision string
	// UID and Version are what the cluster held when it was read, so a sweep deletes nothing
	// Flux has rewritten since.
	UID     string
	Version string
}

// Gate answers Flagger's three questions.
type Gate struct {
	cluster Cluster
	traffic Traffic
	public  Traffic

	mu sync.Mutex
	// askedAt is the revision each member last asked a question at, by namespace and Process.
	askedAt map[string]string
	// standing is what the last round of tending found.
	standing Standing

	// served is the last answer of GET /releases.
	served served
}

// Option changes how a Gate is built.
type Option func(*Gate)

// WithTraffic has the Gate send its own requests through traffic rather than HTTPTraffic.
func WithTraffic(traffic Traffic) Option { return func(g *Gate) { g.traffic = traffic } }

// WithPublicTraffic has the Gate warm up through public rather than PublicTraffic.
func WithPublicTraffic(public Traffic) Option { return func(g *Gate) { g.public = public } }

// New returns a Gate reading cluster.
func New(cluster Cluster, options ...Option) *Gate {
	g := &Gate{cluster: cluster, traffic: HTTPTraffic(), public: PublicTraffic(), askedAt: map[string]string{}, served: served{now: time.Now}}
	for _, option := range options {
		option(g)
	}
	return g
}

func (g *Gate) noteAsked(q Question) {
	g.mu.Lock()
	g.askedAt[q.Namespace+"/"+q.Process] = q.Revision
	g.mu.Unlock()
}

// askedHere reports whether the member has asked a question at the revision.
func (g *Gate) askedHere(namespace, process, revision string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.askedAt[namespace+"/"+process] == revision
}

var errNotAMember = errors.New("gate: the Process is no member of the Application's release gate")

// ErrNoCanary is what Cluster.Canary returns for a Canary that is not there.
var ErrNoCanary = errors.New("gate: no such Canary")

// ErrNoInputs is the error Cluster.Inputs wraps where the Application carries no release-gate
// inputs.
var ErrNoInputs = errors.New("gate: no release-gate inputs")

// reading is what every answer starts from: the Application's release-gate inputs, of which the
// asking Process is a member, and that member's Canary.
type reading struct {
	gate   resolved.ReleaseGate
	canary Canary
}

// stale is the answer to a question about a revision the member's Canary does not carry.
func stale(process string) Answer { return no("%s is not at this revision", process) }

// ask reads the asking member's Canary and then the inputs. current is false where the question
// is not about the render that is applied: its Canary carries another revision, or there is no
// such Canary. The two read the same from outside, and nothing more is read or said, so a
// caller that cannot read the Canary learns neither what is deployed nor whether anything is.
func (g *Gate) ask(ctx context.Context, q Question) (read reading, current bool, err error) {
	read.canary, err = g.cluster.Canary(ctx, q.Namespace, q.Process)
	if errors.Is(err, ErrNoCanary) {
		return reading{}, false, nil
	}
	if err != nil {
		return reading{}, false, fmt.Errorf("gate: read the Canary of %s: %w", q.Process, err)
	}
	if read.canary.Revision != q.Revision {
		return reading{}, false, nil
	}
	// A Canary of another Application is not this question's, whatever the caller says.
	if read.canary.Application != q.Application {
		return reading{}, false, errNotAMember
	}
	raw, err := g.cluster.Inputs(ctx, q.Namespace, q.Application)
	if err != nil {
		return reading{}, false, fmt.Errorf("gate: read the release-gate inputs: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &read.gate); err != nil {
		return reading{}, false, fmt.Errorf("gate: the release-gate inputs do not parse: %w", err)
	}
	for _, m := range read.gate.Members {
		if m.Process == q.Process {
			g.noteAsked(q)
			return read, true, nil
		}
	}
	return reading{}, false, errNotAMember
}

// tag is the part of an Application revision its release Jobs are named by.
func tag(revision string) (string, error) {
	_, hex, found := strings.Cut(revision, ":")
	if !found || len(hex) < tagLength {
		return "", fmt.Errorf("gate: %q is not an Application revision", revision)
	}
	return hex[:tagLength], nil
}

// downInfix is what the Job that undoes a migration carries between the migration identity and
// the revision's tag. It is rendered beside the migration and is no step of a release.
const downInfix = "-down-"

// MayStart answers whether the member's new version may start: once the Application's migration
// and every prepare Process of this revision have completed, and every Process the member waits
// for has started the version it holds.
//
// The inputs do not name a release's steps, so the gate reads them off the Application's Jobs:
// every identity that has ever run a release Job of it is a step, and its Job of this revision
// must be there and complete. A Job that is not applied yet is therefore a step not done, never
// a step the release does not have.
func (g *Gate) MayStart(ctx context.Context, q Question) (Answer, error) {
	read, current, err := g.ask(ctx, q)
	if err != nil {
		return Answer{}, err
	}
	if !current {
		return stale(q.Process), nil
	}
	revision, err := tag(q.Revision)
	if err != nil {
		return Answer{}, err
	}
	jobs, err := g.cluster.Jobs(ctx, q.Namespace, q.Application)
	if err != nil {
		return Answer{}, fmt.Errorf("gate: read the release Jobs: %w", err)
	}
	steps := stepsOf(jobs, revision)
	// A migration the inputs name is a step whether or not its Job is applied yet.
	if migration := read.gate.Migration; migration != nil {
		if _, known := steps[migration.Identity]; !known {
			steps[migration.Identity] = nil
		}
	}
	for _, step := range slices.Sorted(maps.Keys(steps)) {
		job := steps[step]
		switch {
		case job == nil:
			return no("%s-%s is not there yet", step, revision), nil
		case job.Failed:
			return no("%s failed: the release is held", job.Name), nil
		case !job.Complete:
			return no("%s has not completed", job.Name), nil
		}
	}
	// A stop-start Process the member requires in its own Project stops before it starts again,
	// and is released with the member: its new version would start against nothing
	// (deploy-kit spec/v1/55-delivery.md#release-order).
	providers := waitsFor(read, q.Process)
	waiting, held, err := g.unstarted(ctx, q.Namespace, providers)
	if err != nil {
		return Answer{}, err
	}
	if held {
		return no("%s of %s has not started the version it holds", waiting.Process, waiting.Application), nil
	}
	why := "the migration and every prepare Process completed"
	if len(steps) == 0 {
		why = "the release has no migration and no prepare Process"
	}
	if len(providers) > 0 {
		why += ", and every Process it waits for has started"
	}
	return yes(why), nil
}

// unstarted is the first of the providers that has not started the version it holds, in the
// order the inputs name them; held is false where every one has.
func (g *Gate) unstarted(ctx context.Context, namespace string, providers []resolved.GateProvider) (
	waiting resolved.GateProvider, held bool, err error,
) {
	for _, provider := range providers {
		started, err := g.cluster.Started(ctx, namespace, provider.Process)
		if err != nil {
			return resolved.GateProvider{}, false, fmt.Errorf("gate: read %s of %s: %w", provider.Process, provider.Application, err)
		}
		if !started {
			return provider, true, nil
		}
	}
	return resolved.GateProvider{}, false, nil
}

// waitsFor is what the asking member waits for before its new version starts, as its inputs
// name it.
func waitsFor(read reading, process string) []resolved.GateProvider {
	for _, m := range read.gate.Members {
		if m.Process == process {
			return m.WaitsFor
		}
	}
	return nil
}

// stepsOf is every step of a release, by the identity it runs as, with its Job of the revision
// where there is one.
func stepsOf(jobs []Job, revision string) map[string]*Job {
	steps := map[string]*Job{}
	for _, job := range jobs {
		if job.Component == "" || strings.HasPrefix(job.Name, job.Component+downInfix) {
			continue
		}
		if _, known := steps[job.Component]; !known {
			steps[job.Component] = nil
		}
		if job.Name == job.Component+"-"+revision {
			steps[job.Component] = &job
		}
	}
	return steps
}

// Checks answers one analysis iteration for the member: every pod of its new copy is ready, none
// has restarted, and the gate's own requests to the new copy stay under the platform's
// thresholds (traffic.go). A member the Canary hands no path to request is held to its pods alone.
func (g *Gate) Checks(ctx context.Context, q Question) (Answer, error) {
	read, current, err := g.ask(ctx, q)
	if err != nil {
		return Answer{}, err
	}
	if !current {
		return stale(q.Process), nil
	}
	pods, err := g.cluster.NewCopy(ctx, q.Namespace, q.Process)
	if err != nil {
		return Answer{}, fmt.Errorf("gate: read the new copy's pods: %w", err)
	}
	if len(pods) == 0 {
		return no("the new copy of %s has no pod", q.Process), nil
	}
	for _, pod := range pods {
		switch {
		case pod.Restarts > 0:
			return no("%s restarted", pod.Name), nil
		case !pod.Ready:
			return no("%s is not ready", pod.Name), nil
		}
	}
	const healthy = "every pod of the new copy is ready and none restarted"
	targets := targetsOf(read.canary)
	if len(targets) == 0 {
		return yes(healthy), nil
	}
	measured, err := g.measure(ctx, q, read, targets)
	if err != nil || !measured.Yes {
		return measured, err
	}
	return yes(healthy + ", and " + measured.Why), nil
}

// Flagger's phases, as its Canary status spells them.
const (
	phaseWaitingPromotion = "WaitingPromotion"
	phasePromoting        = "Promoting"
	phaseFinalising       = "Finalising"
	phaseProgressing      = "Progressing"
	phaseSucceeded        = "Succeeded"
	phaseInitialized      = "Initialized"
	phaseFailed           = "Failed"
)

// through reports whether a member waits at the barrier or is past it.
func through(c Canary) bool {
	return c.Phase == phaseWaitingPromotion || c.Phase == phasePromoting || c.Phase == phaseFinalising
}

// settled reports whether Flagger records the member as serving what it last saw of it. That is
// half of "did not change": the other half is Cluster.Serves, since Flagger's record is a tick
// behind a Deployment that just changed.
func settled(c Canary) bool {
	return (c.Phase == phaseSucceeded || c.Phase == phaseInitialized) && c.Applied != "" && c.Applied == c.Promoted
}

// MayPromote answers whether the member may be promoted: only once every member of the
// Application has passed its analysis for this revision, or did not change in it. That is the
// barrier.
//
// The member that asks is held to its own record too: Flagger marks it as waiting only after the
// first refusal, so the first time it is still being analysed, with every iteration counted.
func (g *Gate) MayPromote(ctx context.Context, q Question) (Answer, error) {
	read, current, err := g.ask(ctx, q)
	if err != nil {
		return Answer{}, err
	}
	if !current {
		return stale(q.Process), nil
	}
	analysed := read.canary.Phase == phaseProgressing && read.canary.Iterations >= read.gate.Analysis.Iterations
	if !through(read.canary) && !analysed {
		return no("%s has not passed its analysis (%s, %d of %d)", q.Process, read.canary.Phase, read.canary.Iterations, read.gate.Analysis.Iterations), nil
	}
	for _, member := range read.gate.Members {
		if member.Process == q.Process {
			continue
		}
		waits, err := g.holds(ctx, q, member.Process)
		if err != nil {
			return Answer{}, err
		}
		if waits != "" {
			return no("%s", waits), nil
		}
	}
	const passed = "every member passed its analysis or did not change"
	if read.gate.ProtectedPath == nil {
		return yes(passed), nil
	}
	refuses, err := g.refuses(ctx, q, read)
	if err != nil || !refuses.Yes {
		return refuses, err
	}
	return yes(passed + ", and " + refuses.Why), nil
}

// refuses asks the member that serves a native Client's protected path for it, as a stranger
// would: with no credentials, and following no redirect (deploy-kit
// spec/v1/55-delivery.md#the-release-gate). A 2xx is E_CLIENT_OPEN, and no answer is no. A member
// that did not change in the release is not asked: its image was asked when it last did.
func (g *Gate) refuses(ctx context.Context, q Question, read reading) (Answer, error) {
	path, by := *read.gate.ProtectedPath, read.gate.ProtectedBy
	if by == nil || !slices.ContainsFunc(read.gate.Members, func(m resolved.GateMember) bool { return m.Process == *by }) {
		return Answer{}, fmt.Errorf("gate: the inputs name no member that serves the protected path %q", path)
	}
	if !requestable.MatchString(path) {
		return Answer{}, fmt.Errorf("gate: the inputs hand a protected path the gate does not request: %q", path)
	}
	canary, unchanged, err := g.protector(ctx, q, read, *by)
	if err != nil {
		return Answer{}, err
	}
	if unchanged {
		return yes(*by + ", which did not change, was asked when it last did"), nil
	}
	if canary.Port <= 0 || canary.Port > 65535 {
		return Answer{}, fmt.Errorf("%w: %s", errNoPort, *by)
	}
	url := fmt.Sprintf("http://%s-canary.%s.svc.cluster.local:%d%s", *by, q.Namespace, canary.Port, path)
	status, _, err := g.traffic.Get(ctx, url)
	switch {
	case err != nil:
		//nolint:nilerr // no answer is the answer no, not a question the gate could not answer
		return no("%s did not answer %s, so it is not shown to refuse it", *by, path), nil
	case status >= 200 && status < 300:
		return no("E_CLIENT_OPEN: %s serves %s to anyone (%d)", *by, path, status), nil
	}
	return yes(fmt.Sprintf("%s refuses %s to a stranger (%d)", *by, path, status)), nil
}

// holds says what, if anything, another member of the Application holds the barrier shut with:
// empty where it has passed its analysis for the revision or did not change in it.
func (g *Gate) holds(ctx context.Context, q Question, process string) (string, error) {
	canary, err := g.cluster.Canary(ctx, q.Namespace, process)
	if err != nil {
		return "", fmt.Errorf("gate: read the Canary of %s: %w", process, err)
	}
	if canary.Application != q.Application {
		return "", fmt.Errorf("gate: the Canary of %s is not of this Application", process)
	}
	switch {
	// Its Canary is still an earlier render's: nobody has asked it about this revision.
	case canary.Revision != q.Revision:
		return stale(process).Why, nil
	case through(canary):
		// Flagger's status carries no revision: a member seen waiting may be waiting from the
		// release before. One that has asked at this revision is being switched at it.
		if !g.askedHere(q.Namespace, process, q.Revision) {
			return process + " has not asked at this revision yet", nil
		}
		return "", nil
	case !settled(canary):
		return fmt.Sprintf("%s has not passed its analysis (%s)", process, canary.Phase), nil
	}
	serves, err := g.cluster.Serves(ctx, q.Namespace, process)
	if err != nil {
		return "", fmt.Errorf("gate: compare %s with its primary: %w", process, err)
	}
	if !serves {
		return process + " changed and has not started its analysis", nil
	}
	return "", nil
}

// protector reads the Canary of the member that serves the protected path, and says whether it
// did not change in the release: Flagger records it as settled and its primary runs what its
// Deployment holds. The member that asks is being promoted, so it changed.
func (g *Gate) protector(ctx context.Context, q Question, read reading, by string) (Canary, bool, error) {
	if by == q.Process {
		return read.canary, false, nil
	}
	canary, err := g.cluster.Canary(ctx, q.Namespace, by)
	if err != nil {
		return Canary{}, false, fmt.Errorf("gate: read the Canary of %s: %w", by, err)
	}
	if !settled(canary) {
		return canary, false, nil
	}
	serves, err := g.cluster.Serves(ctx, q.Namespace, by)
	if err != nil {
		return Canary{}, false, fmt.Errorf("gate: compare %s with its primary: %w", by, err)
	}
	return canary, serves, nil
}
