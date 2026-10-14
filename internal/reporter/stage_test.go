package reporter_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/JorisJonkers-dev/delivery/internal/reporter"
)

const (
	pinned  = "sha256:9f295c35c049e386644cb3c4b8ec39d6f026fa5a4818583bdeab97c6cd00d809"
	earlier = "sha256:45a3ac53054ddbcc9cd1bf28069396fa83594c02e8f5ec3b3e53232a56ef6bbd"
)

// began is when the Rollout's Deployment was created.
var began = time.Date(2026, 10, 14, 17, 36, 0, 0, time.UTC) //nolint:gochecknoglobals // a fixed instant

func unit(applied string, members ...reporter.Member) reporter.Unit {
	return reporter.Unit{
		Name: "apps-notes", Project: "notes",
		Rollout: reporter.Rollout{Repository: "JorisJonkers-dev/notes", SHA: strings.Repeat("a", 40), Environment: "production", Version: "1.4.2"},
		Pinned:  pinned, Applied: applied, Attempted: applied,
		Ready:   reporter.Condition{Status: "True", Reason: "ReconciliationSucceeded"},
		Members: members,
	}
}

// promoted is a member Flagger promoted in this release.
func promoted(process string) reporter.Member {
	return reporter.Member{Process: process, Phase: "Succeeded", Applied: "h2", Promoted: "h2", Transitioned: began.Add(4 * time.Minute), Serves: true}
}

func TestEachStageTheClusterShows(t *testing.T) {
	cases := map[string]struct {
		unit  reporter.Unit
		want  reporter.Stage
		write bool
	}{
		"Flux has not applied the release yet": {
			unit: unit(earlier),
		},
		"no pin read yet": {
			unit: func() reporter.Unit { u := unit(""); u.Pinned = ""; return u }(),
		},
		"applied, with no blue-green member": {
			unit:  unit(pinned),
			want:  reporter.Stage{State: "success", Description: "notes 1.4.2 applied by Flux and ready"},
			write: true,
		},
		"applied, as Flux spells a tagged revision": {
			unit:  unit("latest@" + pinned),
			want:  reporter.Stage{State: "success", Description: "notes 1.4.2 applied by Flux and ready"},
			write: true,
		},
		"Flux failed its health check on this release": {
			unit: func() reporter.Unit {
				u := unit(earlier)
				u.Attempted = pinned
				u.Ready = reporter.Condition{Status: "False", Reason: "HealthCheckFailed", Message: "timeout waiting for: [Deployment/notes-system/notes status: 'InProgress']"}
				return u
			}(),
			want:  reporter.Stage{State: "failure", Description: "Flux could not apply notes 1.4.2: HealthCheckFailed: timeout waiting for: [Deployment/notes-system/notes status: 'InProgress']"},
			write: true,
		},
		"Flux failed an earlier release, not this one": {
			unit: func() reporter.Unit {
				u := unit(earlier)
				u.Ready = reporter.Condition{Status: "False", Reason: "HealthCheckFailed", Message: "earlier"}
				return u
			}(),
		},
		"Flux waits on another unit": {
			unit: func() reporter.Unit {
				u := unit(earlier)
				u.Attempted = pinned
				u.Ready = reporter.Condition{Status: "False", Reason: "DependencyNotReady", Message: "dependency 'flux-system/estate-vso-secrets' is not ready"}
				return u
			}(),
		},
		"a member under analysis": {
			unit: unit(pinned,
				reporter.Member{Process: "notes-api", Phase: "Progressing", Iteration: 2, Iterations: 5, Transitioned: began.Add(time.Minute)},
				promoted("notes-web")),
			want:  reporter.Stage{State: "in_progress", Description: "applied by Flux; notes-api checks 2/5"},
			write: true,
		},
		"members at the barrier, promoting, and not started": {
			unit: unit(pinned,
				reporter.Member{Process: "notes-api", Phase: "WaitingPromotion"},
				reporter.Member{Process: "notes-web", Phase: "Promoting"},
				reporter.Member{Process: "notes-worker", Phase: "Succeeded", Applied: "h1", Promoted: "h1", Serves: false}),
			want:  reporter.Stage{State: "in_progress", Description: "applied by Flux; notes-api waits for the other members, notes-web promoting, notes-worker waits for Flagger"},
			write: true,
		},
		"every member promoted or unchanged": {
			unit: unit(pinned,
				promoted("notes-api"),
				reporter.Member{Process: "notes-web", Phase: "Initialized", Applied: "h1", Promoted: "h1", Transitioned: began.Add(-time.Hour), Serves: true}),
			want:  reporter.Stage{State: "success", Description: "notes 1.4.2 promoted"},
			write: true,
		},
		"a member Flagger rolled back": {
			unit: unit(pinned,
				promoted("notes-web"),
				reporter.Member{Process: "notes-api", Phase: "Failed", Transitioned: began.Add(6 * time.Minute), Message: "Canary analysis failed, Deployment scaled to zero."}),
			want:  reporter.Stage{State: "failure", Description: "notes-api rolled back: Canary analysis failed, Deployment scaled to zero."},
			write: true,
		},
		"a member rolled back with nothing said": {
			unit:  unit(pinned, reporter.Member{Process: "notes-api", Phase: "Failed", Transitioned: began.Add(time.Minute)}),
			want:  reporter.Stage{State: "failure", Description: "notes-api rolled back: Flagger failed its analysis"},
			write: true,
		},
		"a member still failed from the release before": {
			unit:  unit(pinned, reporter.Member{Process: "notes-api", Phase: "Failed", Transitioned: began.Add(-time.Hour)}),
			want:  reporter.Stage{State: "in_progress", Description: "applied by Flux; notes-api still failed from an earlier release"},
			write: true,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, write := reporter.StageOf(c.unit, began)
			if write != c.write || got != c.want {
				t.Fatalf("StageOf = %+v, %v; want %+v, %v", got, write, c.want, c.write)
			}
		})
	}
}

func TestADescriptionFitsWhatGitHubKeeps(t *testing.T) {
	u := unit(earlier)
	u.Attempted = pinned
	u.Ready = reporter.Condition{Status: "False", Reason: "BuildFailed", Message: strings.Repeat("é", 300)}
	got, _ := reporter.StageOf(u, began)
	if n := utf8.RuneCountInString(got.Description); n != 140 || !strings.HasSuffix(got.Description, "…") {
		t.Fatalf("%d runes: %q", n, got.Description)
	}
}

func TestSettledIsFlaggerSayingTheMemberServesWhatItLastAnalysed(t *testing.T) {
	cases := map[string]struct {
		member reporter.Member
		want   bool
	}{
		"succeeded":                 {reporter.Member{Phase: "Succeeded", Applied: "h", Promoted: "h"}, true},
		"initialized":               {reporter.Member{Phase: "Initialized", Applied: "h", Promoted: "h"}, true},
		"succeeded, not yet copied": {reporter.Member{Phase: "Succeeded", Applied: "h2", Promoted: "h1"}, false},
		"nothing recorded":          {reporter.Member{Phase: "Succeeded"}, false},
		"progressing":               {reporter.Member{Phase: "Progressing", Applied: "h", Promoted: "h"}, false},
	}
	for name, c := range cases {
		if got := reporter.Settled(c.member); got != c.want {
			t.Errorf("%s: Settled = %v", name, got)
		}
	}
}
