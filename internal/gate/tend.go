package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/deploykit/resolved"
)

// Tending is what the gate does when nobody asks it anything
// (deploy-kit spec/v1/55-delivery.md#the-release-gate): once a release has failed Flagger asks no
// more questions, so the gate looks at every gated Application on its own clock. It records what
// each one serves, starts a migration whose proof holds, undoes one only under every condition of
// the Down, and says which Applications are held.

// Standing returns what the last round of tending found.
func (g *Gate) Standing() Standing {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.standing
}

// Gated names one Application that carries release-gate inputs.
type Gated struct {
	Namespace   string
	Application string
}

// Record is the gate's one piece of state per Application, kept in a ConfigMap of its own in the
// gate's namespace, where no Application can write it.
type Record struct {
	// Namespace is the Application's: a record read for another namespace is not this one's.
	Namespace string `json:"namespace"`
	// Serving is the last revision under which every member's primary ran what the render held
	// for it: what the Application serves. Empty where nothing has served under the model.
	Serving string `json:"serving,omitempty"`
	// Promoted is Flagger's digest of each member's primary when Serving was recorded. A primary
	// whose digest has moved since no longer runs Serving.
	Promoted map[string]string `json:"promoted,omitempty"`
	// Pinned is the revision the gate last saw applied and not yet serving, and Since is when
	// it first saw it: the start of that release as far as the gate can tell.
	Pinned string    `json:"pinned,omitempty"`
	Since  time.Time `json:"since,omitzero"`
}

// ErrNoRecord is what Cluster.Record returns where the gate has recorded nothing yet.
var ErrNoRecord = errors.New("gate: no release record")

// Why a release is held.
const (
	// HeldStaleProof: the proof names a revision that no longer serves; the schema is untouched.
	HeldStaleProof = "stale-proof"
	// HeldMigration: the migration failed before any new version started.
	HeldMigration = "migration-failed"
	// HeldAnalysis: a member did not pass its analysis, and Flagger took its new copy away.
	HeldAnalysis = "analysis-failed"
)

// The condition of the Down that did not hold, or what became of a Down that ran.
const (
	UndoFirstRelease     = "first-release"
	UndoNewCopyRunning   = "new-copy-running"
	UndoPrimariesMoved   = "primaries-moved"
	UndoNonTransactional = "non-transactional"
	UndoFailed           = "down-failed"
)

// Held is one held release: the Application serves one revision while its pin names another.
type Held struct {
	Gated
	Serving string
	Pinned  string
	Reason  string
}

// Refused is one migration the gate will not undo, or could not: an urgent alert.
type Refused struct {
	Gated
	// Tag is the tag the Down would return to; empty on a first release, which has none.
	Tag       string
	Condition string
}

// Standing is what one round of tending found across every gated Application.
type Standing struct {
	At      time.Time
	Held    []Held
	Refused []Refused
	// Unanswerable is every Application the gate could not read: it answers no for each.
	Unanswerable []Gated
}

// Tend looks at every gated Application once, acts on each, says in the log what is held and
// what stays applied, and keeps what it found.
// One Application it cannot read does not stop it reading the next.
func (g *Gate) Tend(ctx context.Context, now time.Time, logger *slog.Logger) error {
	applications, err := g.cluster.Applications(ctx)
	if err != nil {
		return fmt.Errorf("gate: list the gated Applications: %w", err)
	}
	found := Standing{At: now}
	// Each Application is tended in its own namespace alone: its record is named for that
	// namespace, and every Job it starts is there. Inputs under its id elsewhere are tended as
	// what they are, another namespace's, and change nothing of this one.
	for _, a := range applications {
		held, refused, err := g.tend(ctx, a, now)
		if err != nil {
			logger.ErrorContext(ctx, "release gate could not tend an Application", "namespace", a.Namespace, "application", a.Application, "error", err)
			found.Unanswerable = append(found.Unanswerable, a)
			continue
		}
		if held != nil {
			logger.WarnContext(ctx, "release held", "namespace", a.Namespace, "application", a.Application, "serving", held.Serving, "pinned", held.Pinned, "reason", held.Reason)
			found.Held = append(found.Held, *held)
		}
		if refused != nil {
			logger.ErrorContext(ctx, "migration not undone", "namespace", a.Namespace, "application", a.Application, "tag", refused.Tag, "condition", refused.Condition)
			found.Refused = append(found.Refused, *refused)
		}
	}
	g.mu.Lock()
	g.standing = found
	g.mu.Unlock()
	return nil
}

// Run tends on every tick until ctx ends.
func (g *Gate) Run(ctx context.Context, ticks <-chan time.Time, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticks:
			if err := g.Tend(ctx, now, logger); err != nil {
				logger.ErrorContext(ctx, "release gate could not tend", "error", err)
			}
		}
	}
}

// migrationIdentity is the identity an Application's migration runs as
// (deploy-kit spec/v1/20-resolved-deployment.md#the-migration).
func migrationIdentity(application string) string { return application + "-migration" }

// release is one Application as a round of tending reads it.
type release struct {
	Gated
	inputs  resolved.ReleaseGate
	members map[string]Canary
	pinned  string
	record  Record
}

// read gathers the Application's inputs, its members' Canaries and its record. settling is true
// while the members' Canaries carry different revisions: a render is being applied, and there is
// nothing to conclude until it is.
func (g *Gate) read(ctx context.Context, a Gated) (r release, settling bool, err error) {
	r.Gated = a
	raw, err := g.cluster.Inputs(ctx, a.Namespace, a.Application)
	if err != nil {
		return r, false, fmt.Errorf("read the release-gate inputs: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &r.inputs); err != nil {
		return r, false, fmt.Errorf("the release-gate inputs do not parse: %w", err)
	}
	r.members = map[string]Canary{}
	for _, m := range r.inputs.Members {
		canary, err := g.cluster.Canary(ctx, a.Namespace, m.Process)
		if err != nil {
			return r, false, fmt.Errorf("read the Canary of %s: %w", m.Process, err)
		}
		if canary.Application != a.Application {
			return r, false, fmt.Errorf("the Canary of %s is not of this Application", m.Process)
		}
		if r.pinned != "" && canary.Revision != r.pinned {
			return r, true, nil
		}
		r.pinned = canary.Revision
		r.members[m.Process] = canary
	}
	r.record, err = g.cluster.Record(ctx, a.Namespace, a.Application)
	if err != nil && !errors.Is(err, ErrNoRecord) {
		return r, false, fmt.Errorf("read the release record: %w", err)
	}
	return r, false, nil
}

// serves reports whether every member's primary runs what the render holds for it: Flagger
// records it as settled, and the two Deployments agree.
func (g *Gate) serves(ctx context.Context, r release) (bool, error) {
	for _, process := range slices.Sorted(maps.Keys(r.members)) {
		if !settled(r.members[process]) {
			return false, nil
		}
		same, err := g.cluster.Serves(ctx, r.Namespace, process)
		if err != nil {
			return false, fmt.Errorf("compare %s with its primary: %w", process, err)
		}
		if !same {
			return false, nil
		}
	}
	return true, nil
}

// promoted is Flagger's digest of every member's primary.
func (r release) promoted() map[string]string {
	digests := map[string]string{}
	for process, canary := range r.members {
		digests[process] = canary.Promoted
	}
	return digests
}

// unmoved reports whether every primary the record knows still runs what it ran when the serving
// revision was recorded. A member the record does not know is new, and a new member has no
// primary that could run anything older.
func (r release) unmoved() bool {
	for process, digest := range r.record.Promoted {
		if canary, member := r.members[process]; member && canary.Promoted != digest {
			return false
		}
	}
	return true
}

// proven reports whether the release's compatibility proof still holds
// (deploy-kit spec/v1/55-delivery.md#migration-safety): every primary runs the revision the
// proof ran against. A release proven against nothing starts only where nothing serves.
func (r release) proven() bool {
	tested := r.inputs.Migration.TestedAgainst
	if tested == nil {
		return r.record.Serving == ""
	}
	return r.record.Serving == *tested && r.unmoved()
}

// tend acts on one Application and says whether its release is held, and whether a migration of
// it stays applied that the gate would otherwise have undone.
func (g *Gate) tend(ctx context.Context, a Gated, now time.Time) (*Held, *Refused, error) {
	r, settling, err := g.read(ctx, a)
	if err != nil || settling {
		return nil, nil, err
	}
	serving, err := g.serves(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	if serving {
		return nil, nil, g.record(ctx, r, Record{Serving: r.pinned, Promoted: r.promoted()})
	}
	// A release of the pinned revision is under way, or held.
	if r, err = g.note(ctx, r, now); err != nil {
		return nil, nil, err
	}
	reason, up, down, err := g.migrate(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	if reason == "" {
		reason = r.failed()
	}
	if reason == "" {
		return nil, nil, nil
	}
	held := &Held{Gated: a, Serving: r.record.Serving, Pinned: r.pinned, Reason: reason}
	// Only a migration that ran can be undone: a stale proof left the schema untouched.
	if up == nil || (!up.Complete && !up.Failed) {
		return held, nil, nil
	}
	condition, err := g.undo(ctx, r, down)
	if err != nil || condition == "" {
		return held, nil, err
	}
	return held, r.refusal(condition), nil
}

// note records when the gate first saw the pinned revision applied and not yet serving.
func (g *Gate) note(ctx context.Context, r release, now time.Time) (release, error) {
	if r.record.Pinned == r.pinned {
		return r, nil
	}
	noted := r.record
	noted.Pinned, noted.Since = r.pinned, now
	if err := g.record(ctx, r, noted); err != nil {
		return r, err
	}
	r.record = noted
	return r, nil
}

// refusal is the migration the gate leaves applied, with the tag its Down would return to.
func (r release) refusal(condition string) *Refused {
	refused := &Refused{Gated: r.Gated, Condition: condition}
	if tested := r.inputs.Migration.TestedAgainst; tested != nil {
		// A revision the inputs carry is one the render wrote, so it has a tag.
		refused.Tag, _ = tag(*tested)
	}
	return refused
}

// record writes the Application's record where it differs from what the cluster holds.
func (g *Gate) record(ctx context.Context, r release, next Record) error {
	if next.Serving == r.record.Serving && next.Pinned == r.record.Pinned && next.Since.Equal(r.record.Since) && maps.Equal(next.Promoted, r.record.Promoted) {
		return nil
	}
	if err := g.cluster.SetRecord(ctx, r.Namespace, r.Application, next); err != nil {
		return fmt.Errorf("write the release record: %w", err)
	}
	return nil
}

// migrate starts the release's migration where its proof holds, and says why the release is
// held where the proof went stale or the migration failed. It returns the up and the down Job
// of the pinned revision, where the Application has a migration and they are applied.
func (g *Gate) migrate(ctx context.Context, r release) (reason string, up, down *Job, err error) {
	if r.inputs.Migration == nil {
		return "", nil, nil, nil
	}
	revision, err := tag(r.pinned)
	if err != nil {
		return "", nil, nil, err
	}
	jobs, err := g.cluster.Jobs(ctx, r.Namespace, r.Application)
	if err != nil {
		return "", nil, nil, fmt.Errorf("read the release Jobs: %w", err)
	}
	identity := r.inputs.Migration.Identity
	// The identity is a fixed function of the Application id: inputs that name another would have
	// the gate start a Job that is not this Application's migration.
	if identity != migrationIdentity(r.Application) {
		return "", nil, nil, fmt.Errorf("the inputs name %s as the migration identity of %s", identity, r.Application)
	}
	for _, job := range jobs {
		switch job.Name {
		case identity + "-" + revision:
			up = &job
		case identity + downInfix + revision:
			down = &job
		}
	}
	switch {
	case up == nil:
		// Not applied yet: nothing has run, and nothing is held.
		return "", nil, down, nil
	case up.Failed:
		return HeldMigration, up, down, nil
	case up.Complete || !up.Suspended:
		return "", up, down, nil
	case !r.proven():
		return HeldStaleProof, up, down, nil
	}
	if err := g.cluster.Start(ctx, r.Namespace, *up); err != nil {
		return "", nil, nil, fmt.Errorf("start %s: %w", up.Name, err)
	}
	return "", up, down, nil
}

// failed says why a release that started is held: a member Flagger failed since the gate first
// saw this revision. Flagger's status carries no revision, so a failure it recorded before this
// one was applied is the release before's.
func (r release) failed() string {
	for _, canary := range r.members {
		if canary.Phase == phaseFailed && canary.Transitioned.After(r.record.Since) {
			return HeldAnalysis
		}
	}
	return ""
}

// undo unsuspends the Down where every one of its conditions holds
// (deploy-kit spec/v1/55-delivery.md#failure-and-undo), and otherwise says which did not: the
// release is held, every member's new copy is at zero, every primary runs the revision the
// release was proven against, and no changeset of it ran outside a transaction. A Down that
// failed is reported and never started again. An empty condition is a Down that is started, was
// started before, or has run.
func (g *Gate) undo(ctx context.Context, r release, down *Job) (condition string, err error) {
	switch {
	case down == nil:
		return UndoFirstRelease, nil
	case down.Failed:
		return UndoFailed, nil
	case down.Complete || !down.Suspended:
		return "", nil
	}
	for _, process := range slices.Sorted(maps.Keys(r.members)) {
		pods, err := g.cluster.NewCopy(ctx, r.Namespace, process)
		if err != nil {
			return "", fmt.Errorf("read the new copy of %s: %w", process, err)
		}
		if len(pods) > 0 {
			return UndoNewCopyRunning, nil
		}
	}
	switch {
	case !r.proven():
		return UndoPrimariesMoved, nil
	case r.inputs.Migration.NonTransactional:
		return UndoNonTransactional, nil
	}
	if err := g.cluster.Start(ctx, r.Namespace, *down); err != nil {
		return "", fmt.Errorf("start %s: %w", down.Name, err)
	}
	return "", nil
}
