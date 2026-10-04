// Package gate is the Release Gate's decisions: whether a member's new version may start, whether
// it passes an analysis iteration, and whether it may be promoted
// (deploy-kit spec/v1/55-delivery.md#the-release-gate).
//
// The gate keeps no state. Every answer is read from the cluster at the moment it is asked: the
// Application's release-gate inputs, its release Jobs, its members' Canaries and Deployments,
// and the pods of the new copy. A gate that restarts mid-release answers the next question as
// it would have. And it fails closed: an answer it cannot read is no.
//
// It answers a question only at the revision the asking member's Canary carries. A revision is
// a digest nobody guesses, written in the Canary and nowhere a stranger reads, so a caller that
// cannot read the Canary learns nothing from the gate, and a question about a render that has
// been replaced is refused.
package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

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
	// Inputs returns the Application's release-gate ConfigMap's one value.
	Inputs(ctx context.Context, namespace, application string) (string, error)
	// Jobs returns the Jobs of the Application: every one labelled as part of it.
	Jobs(ctx context.Context, namespace, application string) ([]Job, error)
	// Canary returns the member's Canary.
	Canary(ctx context.Context, namespace, process string) (Canary, error)
	// NewCopy returns the pods of the member's new copy, never its primary's.
	NewCopy(ctx context.Context, namespace, process string) ([]Pod, error)
	// Serves reports whether the member's primary runs what its Deployment now holds: the same
	// containers, images, commands, arguments and variables. It is read off the two Deployments,
	// not off Flagger's record, which is a tick behind a Deployment that just changed.
	Serves(ctx context.Context, namespace, process string) (bool, error)
}

// Gate answers Flagger's three questions.
type Gate struct {
	cluster Cluster
}

// New returns a Gate reading cluster.
func New(cluster Cluster) *Gate { return &Gate{cluster: cluster} }

var errNotAMember = errors.New("gate: the Process is no member of the Application's release gate")

// reading is what every answer starts from: the Application's release-gate inputs, of which the
// asking Process is a member, and that member's Canary.
type reading struct {
	gate   resolved.ReleaseGate
	canary Canary
}

// stale is the answer to a question about a revision the member's Canary does not carry.
func stale(process string) Answer { return no("%s is not at this revision", process) }

// ask reads the inputs and the asking member's Canary. current is false where the Canary
// carries another revision than the question: the render the question is about is not the one
// applied, and nothing more is read or said.
func (g *Gate) ask(ctx context.Context, q Question) (read reading, current bool, err error) {
	raw, err := g.cluster.Inputs(ctx, q.Namespace, q.Application)
	if err != nil {
		return reading{}, false, fmt.Errorf("gate: read the release-gate inputs: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &read.gate); err != nil {
		return reading{}, false, fmt.Errorf("gate: the release-gate inputs do not parse: %w", err)
	}
	member := false
	for _, m := range read.gate.Members {
		member = member || m.Process == q.Process
	}
	if !member {
		return reading{}, false, errNotAMember
	}
	if read.canary, err = g.cluster.Canary(ctx, q.Namespace, q.Process); err != nil {
		return reading{}, false, fmt.Errorf("gate: read the Canary of %s: %w", q.Process, err)
	}
	// A Canary of another Application is not this question's, whatever the caller says.
	if read.canary.Application != q.Application {
		return reading{}, false, errNotAMember
	}
	return read, read.canary.Revision == q.Revision, nil
}

// tag is the part of an Application revision its release Jobs are named by.
func tag(revision string) (string, error) {
	_, hex, found := strings.Cut(revision, ":")
	if !found || len(hex) < tagLength {
		return "", fmt.Errorf("gate: %q is not an Application revision", revision)
	}
	return hex[:tagLength], nil
}

// down is the suffix of the identity a migration is undone as. The Job that runs as it is
// rendered beside the migration and is no step of a release.
const down = "-migration-down"

// MayStart answers whether the member's new version may start: once the Application's migration
// and every prepare Process of this revision have completed.
//
// The inputs do not name a release's steps, so the gate reads them off the Application's Jobs:
// every identity that has ever run a release Job of it is a step, and its Job of this revision
// must be there and complete. A Job that is not applied yet is therefore a step not done, never
// a step the release does not have.
func (g *Gate) MayStart(ctx context.Context, q Question) (Answer, error) {
	_, current, err := g.ask(ctx, q)
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
	if len(steps) == 0 {
		return yes("the release has no migration and no prepare Process"), nil
	}
	return yes("the migration and every prepare Process completed"), nil
}

// stepsOf is every step of a release, by the identity it runs as, with its Job of the revision
// where there is one.
func stepsOf(jobs []Job, revision string) map[string]*Job {
	steps := map[string]*Job{}
	for _, job := range jobs {
		if job.Component == "" || strings.HasSuffix(job.Component, down) {
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

// Checks answers one analysis iteration for the member: every pod of its new copy is ready, and
// none has restarted. What `error-rate` and `latency` measure is not decided yet, so a member
// that carries them is held to the same two facts as one that does not.
func (g *Gate) Checks(ctx context.Context, q Question) (Answer, error) {
	_, current, err := g.ask(ctx, q)
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
	return yes("every pod of the new copy is ready and none restarted"), nil
}

// Flagger's phases, as its Canary status spells them.
const (
	phaseWaitingPromotion = "WaitingPromotion"
	phasePromoting        = "Promoting"
	phaseFinalising       = "Finalising"
	phaseProgressing      = "Progressing"
	phaseSucceeded        = "Succeeded"
	phaseInitialized      = "Initialized"
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
	return yes("every member passed its analysis or did not change"), nil
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
