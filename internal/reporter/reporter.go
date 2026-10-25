package reporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp"
)

// Cluster is what the Reporter reads: every unit Flux applies for deploy-kit, with its source's
// pin and its Canaries.
type Cluster interface {
	Units(ctx context.Context) ([]Unit, error)
}

// Gate is what the Reporter reads of the Release Gate: what it holds of every gated Application,
// the outcome of each warmup among it.
type Gate interface {
	Releases(ctx context.Context) (gate.Releases, error)
}

// GitHub is where the Reporter writes.
type GitHub interface {
	Find(ctx context.Context, repository, sha, environment string) (githubapp.Deployment, bool, error)
	Latest(ctx context.Context, repository string, id int64) (githubapp.Status, bool, error)
	Post(ctx context.Context, repository string, id int64, status githubapp.Status) error
}

// The states after which a Deployment takes no more statuses from the Reporter: promoted, or
// superseded by a later release.
const (
	stateInactive = "inactive"
	stateError    = "error"
)

// absentFor is how long the Reporter believes a Rollout has no Deployment before it asks again: a
// unit pinned before Deployments were created has none, and asking every pass would spend a
// request on it each time.
const absentFor = 10 * time.Minute

// What a Rollout's annotations may hold before the Reporter puts them in a request: they are the
// fragment's own claims, copied onto the unit.
var (
	repositoryPattern  = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	shaPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Reporter writes each stage a unit's release reaches to its Rollout's Deployment.
type Reporter struct {
	Cluster Cluster
	// Gate is read once a pass for each release's warmup; nil reads none, and a release is then
	// through once every member is.
	Gate   Gate
	GitHub GitHub
	Now    func() time.Time
	Log    *slog.Logger

	// rollouts is what the Reporter knows of each Rollout it has seen, by repository, commit and
	// environment. Only the loop touches it.
	rollouts map[string]*rollout
}

// rollout is the Reporter's memory of one Deployment.
type rollout struct {
	deployment githubapp.Deployment
	// last is the stage the Deployment says, as last read or written.
	last   Stage
	logURL string
	// done is set once the Deployment takes no more statuses.
	done bool
	// absentUntil is set where GitHub holds no Deployment for the Rollout.
	absentUntil time.Time
}

// Pass reads every unit and writes the stage of each one's Rollout where it moved. It reports
// whether it wrote anything. A Rollout it cannot report is logged and the rest still are.
func (r *Reporter) Pass(ctx context.Context) (bool, error) {
	units, err := r.Cluster.Units(ctx)
	if err != nil {
		return false, fmt.Errorf("read the units: %w", err)
	}
	if r.rollouts == nil {
		r.rollouts = map[string]*rollout{}
	}
	r.addWarmups(ctx, units)
	seen := map[string]bool{}
	wrote := false
	var errs []error
	for _, u := range units {
		// The estate's own units carry no Rollout: there is nothing to write for them.
		if u.Rollout == (Rollout{}) {
			continue
		}
		if err := valid(u.Rollout); err != nil {
			r.Log.WarnContext(ctx, "a unit's Rollout cannot be reported", "unit", u.Name, "error", err)
			continue
		}
		// A release pinned again at the same commit, as a toolkit change moves every pin, is a
		// new Rollout with a Deployment of its own, so the render it pins is part of its key.
		key := u.Rollout.Repository + "@" + u.Rollout.SHA + "/" + u.Rollout.Environment + "#" + u.Pinned
		seen[key] = true
		written, err := r.report(ctx, key, u)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u.Name, err))
		}
		wrote = wrote || written
	}
	// A Rollout no unit names any more was superseded: forget it.
	for key := range r.rollouts {
		if !seen[key] {
			delete(r.rollouts, key)
		}
	}
	return wrote, errors.Join(errs...)
}

// addWarmups gives every unit what the Release Gate's warmup met, by namespace and Application,
// where the Reporter reads a gate. A gate that does not answer is one whose warmups are not known
// yet: every release it gates waits on them, and none is failed for it.
func (r *Reporter) addWarmups(ctx context.Context, units []Unit) {
	if r.Gate == nil {
		return
	}
	warmups := map[string]gate.Warmup{}
	for i := range units {
		units[i].Warmups = warmups
	}
	releases, err := r.Gate.Releases(ctx)
	if err != nil {
		r.Log.WarnContext(ctx, "the Release Gate's releases cannot be read", "error", err)
		return
	}
	for _, release := range releases.Releases {
		if release.Warmup != nil {
			warmups[release.Namespace+"/"+release.Application] = *release.Warmup
		}
	}
}

func (r *Reporter) report(ctx context.Context, key string, u Unit) (bool, error) {
	known, found, err := r.recall(ctx, key, u)
	if err != nil || !found || known.done {
		return false, err
	}
	stage, ok := StageOf(u, known.deployment.Created)
	if !ok || stage == known.last {
		return false, nil
	}
	status := githubapp.Status{State: stage.State, Description: stage.Description, LogURL: known.logURL, EnvironmentURL: u.Rollout.URL}
	if err := r.GitHub.Post(ctx, u.Rollout.Repository, known.deployment.ID, status); err != nil {
		return false, err
	}
	r.Log.InfoContext(ctx, "reported a stage", "unit", u.Name, "version", u.Rollout.Version, "state", stage.State, "description", stage.Description)
	known.last = stage
	// A promoted release is done; a failed one may still be applied on Flux's next try.
	known.done = stage.State == StateSuccess
	return true, nil
}

// recall returns what the Reporter knows of the Rollout, asking GitHub where it knows nothing. It
// returns false where GitHub holds no Deployment of this release.
func (r *Reporter) recall(ctx context.Context, key string, u Unit) (*rollout, bool, error) {
	known := r.rollouts[key]
	switch {
	case known == nil:
	case known.absentUntil.IsZero():
		return known, true, nil
	case r.Now().Before(known.absentUntil):
		return nil, false, nil
	}
	deployment, found, err := r.GitHub.Find(ctx, u.Rollout.Repository, u.Rollout.SHA, u.Rollout.Environment)
	if err != nil {
		return nil, false, err
	}
	// A Deployment is this Rollout's only where it names the same Project and release.
	if !found || deployment.Project != u.Project || deployment.Version != u.Rollout.Version {
		r.rollouts[key] = &rollout{absentUntil: r.Now().Add(absentFor)}
		r.Log.InfoContext(ctx, "no Deployment for a Rollout", "unit", u.Name, "repository", u.Rollout.Repository, "version", u.Rollout.Version)
		return nil, false, nil
	}
	known = &rollout{deployment: deployment}
	latest, has, err := r.GitHub.Latest(ctx, u.Rollout.Repository, deployment.ID)
	if err != nil {
		return nil, false, err
	}
	if has {
		known.last = Stage{State: latest.State, Description: latest.Description}
		known.logURL = latest.LogURL
		known.done = latest.State == StateSuccess || latest.State == stateInactive || latest.State == stateError
	}
	r.rollouts[key] = known
	return known, true, nil
}

func valid(r Rollout) error {
	switch {
	case !repositoryPattern.MatchString(r.Repository):
		return fmt.Errorf("repository %q is not owner/name", r.Repository)
	case !shaPattern.MatchString(r.SHA):
		return fmt.Errorf("sha %q is not a commit", r.SHA)
	case !environmentPattern.MatchString(r.Environment):
		return fmt.Errorf("environment %q is not a name", r.Environment)
	case r.Version == "":
		return errors.New("no version")
	}
	return nil
}
