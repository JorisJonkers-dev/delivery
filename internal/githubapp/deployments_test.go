package githubapp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/githubapp"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp/githubtest"
)

const (
	notes = "JorisJonkers-dev/notes"
	sha   = "33a64302df71e989c018f6aa4602a8cb070dc739"
)

func deployments(t *testing.T, server *githubtest.Server) githubapp.Deployments {
	t.Helper()
	return githubapp.Deployments{App: file(t, server).App}
}

func TestTheAppFindsTheNewestDeploymentOfACommitAndWritesToIt(t *testing.T) {
	server := githubtest.New(t, "")
	created := time.Date(2026, 10, 14, 17, 36, 53, 0, time.UTC)
	for _, version := range []string{"1.4.1", "1.4.2"} {
		server.Deploy(githubtest.Deployment{Repository: notes, SHA: sha, Environment: "production", Project: "notes", Version: version, Created: created},
			githubtest.Status{State: "in_progress", Description: "notes " + version + " pinned", LogURL: "https://example.invalid/run"})
	}
	d := deployments(t, server)
	ctx := t.Context()

	found, ok, err := d.Find(ctx, notes, sha, "production")
	if err != nil || !ok || found.ID != 2 || found.Project != "notes" || found.Version != "1.4.2" || !found.Created.Equal(created) {
		t.Fatalf("Find = %+v, %v, %v", found, ok, err)
	}
	latest, ok, err := d.Latest(ctx, notes, found.ID)
	if err != nil || !ok || latest != (githubapp.Status{State: "in_progress", Description: "notes 1.4.2 pinned", LogURL: "https://example.invalid/run"}) {
		t.Fatalf("Latest = %+v, %v, %v", latest, ok, err)
	}
	written := githubapp.Status{State: "success", Description: "notes 1.4.2 promoted", LogURL: "https://example.invalid/run", EnvironmentURL: "https://notes.jorisjonkers.dev"}
	if err := d.Post(ctx, notes, found.ID, written); err != nil {
		t.Fatal(err)
	}
	if latest, _, _ := d.Latest(ctx, notes, found.ID); latest != written {
		t.Fatalf("after the write, Latest = %+v", latest)
	}
	// GitHub marks the earlier Deployment inactive once a later one succeeds.
	if latest, _, _ := d.Latest(ctx, notes, 1); latest.State != "inactive" {
		t.Fatalf("the earlier Deployment is %+v", latest)
	}
}

func TestNothingFoundIsNotAnError(t *testing.T) {
	server := githubtest.New(t, "")
	server.Deploy(githubtest.Deployment{Repository: notes, SHA: sha, Environment: "production"}, githubtest.Status{State: "in_progress"})
	d := deployments(t, server)
	ctx := t.Context()
	if _, ok, err := d.Find(ctx, notes, strings.Repeat("b", 40), "production"); ok || err != nil {
		t.Fatalf("another commit: %v, %v", ok, err)
	}
	if _, ok, err := d.Find(ctx, notes, sha, "staging"); ok || err != nil {
		t.Fatalf("another environment: %v, %v", ok, err)
	}
}

func TestADeploymentGitHubDoesNotHoldOrAStatusItRefusesIsAnError(t *testing.T) {
	server := githubtest.New(t, "")
	server.Deploy(githubtest.Deployment{Repository: notes, SHA: sha, Environment: "production"}, githubtest.Status{State: "in_progress"})
	d := deployments(t, server)
	ctx := t.Context()
	if _, _, err := d.Latest(ctx, notes, 9); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Latest of nothing: %v", err)
	}
	if err := d.Post(ctx, notes, 9, githubapp.Status{State: "success"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Post to nothing: %v", err)
	}
	if err := d.Post(ctx, notes, 1, githubapp.Status{State: "success", Description: strings.Repeat("x", 141)}); err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("a description GitHub refuses: %v", err)
	}
}

func TestAnAppGitHubDoesNotKnowCanDoNothingWithDeployments(t *testing.T) {
	server := githubtest.New(t, "")
	d := deployments(t, server)
	d.AppID = "1"
	ctx := t.Context()
	if _, _, err := d.Find(ctx, notes, sha, "production"); err == nil || !strings.Contains(err.Error(), "sign in as the App") {
		t.Fatalf("Find: %v", err)
	}
	if _, _, err := d.Latest(ctx, notes, 1); err == nil {
		t.Fatal("Latest signed in")
	}
	if err := d.Post(ctx, notes, 1, githubapp.Status{State: "success"}); err == nil {
		t.Fatal("Post signed in")
	}
}
