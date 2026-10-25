// Package reporter is the Reporter: it keeps every Rollout's GitHub Deployment current, stage by
// stage, as the cluster moves the release (deploy-kit spec/v1/55-delivery.md#rollouts).
//
// An event Flux sends is a prompt to look and never evidence: the route is unsigned. The Reporter
// therefore reads every unit that carries a Rollout, and its Canaries, from the cluster itself,
// on every prompt and on its own interval, and writes a status only from what it read.
package reporter

import (
	"fmt"
	"strings"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

// Rollout is a release as its unit's own annotations name it.
type Rollout struct {
	Repository  string
	SHA         string
	Environment string
	Version     string
	// URL is where the release is reachable; empty for a Project with no exposure.
	URL string
}

// Condition is the Ready condition of a Flux object.
type Condition struct {
	Status  string
	Reason  string
	Message string
}

// Unit is one Project's unit, as the cluster holds it.
type Unit struct {
	// Name is the Kustomization's, apps-<project>.
	Name    string
	Project string
	Rollout Rollout
	// Pinned is the revision the unit's source is pinned to: the digest of the render its pin
	// names. Applied and Attempted are the revisions Flux last applied, and last tried to.
	Pinned    string
	Applied   string
	Attempted string
	Ready     Condition
	Members   []Member
	// Warmups is what the Release Gate's warmup met, for every gated Application it holds a
	// record of, by `<namespace>/<application>`. Nil where the Reporter reads no gate: a release
	// is then through once every member is.
	Warmups map[string]gate.Warmup
}

// Member is one blue-green Process of the unit, as its Canary records it.
type Member struct {
	// Namespace, Application and Revision are the Canary's own and what its webhooks carry: the
	// Application the member belongs to, and the revision it was rendered at.
	Namespace   string
	Application string
	Revision    string
	Process     string
	Phase       string
	// Iteration is the analysis iteration Flagger has reached, of Iterations.
	Iteration  int
	Iterations int
	// Applied and Promoted are Flagger's digests of what it last analysed and last promoted.
	Applied  string
	Promoted string
	// Transitioned is when Flagger last moved the Canary to another phase.
	Transitioned time.Time
	// Message is what Flagger's Promoted condition says.
	Message string
	// Serves is whether the member's primary runs what its own Deployment holds: the half of
	// "did not change" Flagger's status cannot say (internal/gate.Kube.Serves).
	Serves bool
}

// Flagger's phases, as its Canary status spells them.
const (
	phaseProgressing      = "Progressing"
	phaseWaitingPromotion = "WaitingPromotion"
	phasePromoting        = "Promoting"
	phaseFinalising       = "Finalising"
	phaseSucceeded        = "Succeeded"
	phaseInitialized      = "Initialized"
	phaseFailed           = "Failed"
)

// The states of a GitHub deployment status the Reporter writes.
const (
	StateInProgress = "in_progress"
	StateSuccess    = "success"
	StateFailure    = "failure"
)

// The reason Flux gives while a unit waits on another; it is waiting, not failing.
const reasonDependencyNotReady = "DependencyNotReady"

// descriptionLimit is the longest description GitHub keeps on a status.
const descriptionLimit = 140

// Stage is what a Rollout's Deployment should say now.
type Stage struct {
	State       string
	Description string
}

// StageOf is the stage the cluster shows the unit's release at. since is when the Rollout began,
// so a Canary's phase is read as this release's only where Flagger reached it after that. It
// returns false where there is nothing to say beyond `pinned`: Flux has not applied the release
// and has not failed to.
func StageOf(u Unit, since time.Time) (Stage, bool) {
	release := u.Project + " " + u.Rollout.Version
	if u.Pinned == "" {
		return Stage{}, false
	}
	if u.Ready.Status == "False" && u.Ready.Reason != reasonDependencyNotReady &&
		(at(u.Applied, u.Pinned) || at(u.Attempted, u.Pinned)) {
		return failure(fmt.Sprintf("Flux could not apply %s: %s: %s", release, u.Ready.Reason, u.Ready.Message)), true
	}
	if !at(u.Applied, u.Pinned) {
		return Stage{}, false
	}
	if len(u.Members) == 0 {
		return Stage{State: StateSuccess, Description: release + " applied by Flux and ready"}, true
	}

	var waiting []string
	for _, m := range u.Members {
		at, done, failed := memberAt(m, since)
		if failed {
			return failure(fmt.Sprintf("%s rolled back: %s", m.Process, orDefault(m.Message, "Flagger failed its analysis"))), true
		}
		if !done {
			waiting = append(waiting, m.Process+" "+at)
		}
	}
	if len(waiting) == 0 {
		return warmedUp(u, release), true
	}
	return Stage{State: StateInProgress, Description: limit("applied by Flux; " + strings.Join(waiting, ", "))}, true
}

// warmedUp is the stage of a release every member of which is through. Where the Reporter reads
// the Release Gate, the release is promoted once the gate has warmed each of its Applications up
// at the revision it serves, and failed where a warmup met a 5xx
// (deploy-kit spec/v1/55-delivery.md#the-release-gate): the gate warms a revision once, and rolls
// nothing back.
func warmedUp(u Unit, release string) Stage {
	if u.Warmups == nil {
		return Stage{State: StateSuccess, Description: release + " promoted"}
	}
	var warming []string
	seen := map[string]bool{}
	for _, m := range u.Members {
		key := m.Namespace + "/" + m.Application
		if seen[key] {
			continue
		}
		seen[key] = true
		w, held := u.Warmups[key]
		switch {
		case !held || w.Revision != m.Revision:
			warming = append(warming, m.Application)
		case w.Failed != "":
			return failure(fmt.Sprintf("%s promoted, but its warmup failed: %s", m.Application, w.Failed))
		}
	}
	if len(warming) > 0 {
		return Stage{State: StateInProgress, Description: limit("promoted; the Release Gate is warming up " + strings.Join(warming, ", "))}
	}
	return Stage{State: StateSuccess, Description: release + " promoted and warmed up"}
}

// memberAt says where one member stands in this release: a few words for its stage, whether it
// is through, and whether Flagger rolled it back.
func memberAt(m Member, since time.Time) (stage string, done, failed bool) {
	switch {
	case m.Phase == phaseFailed && m.Transitioned.After(since):
		return "", false, true
	case m.Phase == phaseProgressing:
		return fmt.Sprintf("checks %d/%d", m.Iteration, m.Iterations), false, false
	case m.Phase == phaseWaitingPromotion:
		return "waits for the other members", false, false
	case m.Phase == phasePromoting || m.Phase == phaseFinalising:
		return "promoting", false, false
	case Settled(m) && m.Serves:
		// Promoted in this release, or unchanged by it: either way its primary serves the render.
		return "", true, false
	case m.Phase == phaseFailed:
		return "still failed from an earlier release", false, false
	default:
		return "waits for Flagger", false, false
	}
}

// Settled reports whether Flagger records the member as serving what it last analysed. That is
// half of "did not change"; the other half is Member.Serves, since Flagger's record is a tick
// behind a Deployment that just changed.
func Settled(m Member) bool {
	return (m.Phase == phaseSucceeded || m.Phase == phaseInitialized) && m.Applied != "" && m.Applied == m.Promoted
}

// at reports whether Flux's revision is the pinned digest. Flux writes an OCI revision as the
// digest alone where the source is pinned by one, and as <tag>@<digest> where it also names a tag.
func at(revision, digest string) bool {
	return revision == digest || strings.HasSuffix(revision, "@"+digest)
}

func failure(description string) Stage {
	return Stage{State: StateFailure, Description: limit(description)}
}

// limit cuts a description to what GitHub keeps, on a rune boundary.
func limit(description string) string {
	runes := []rune(description)
	if len(runes) <= descriptionLimit {
		return description
	}
	return string(runes[:descriptionLimit-1]) + "…"
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
