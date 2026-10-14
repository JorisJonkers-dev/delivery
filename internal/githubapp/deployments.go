package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Deployments reads and writes the GitHub Deployments of every repository the App is installed
// on: the Reporter's whole reach outside the cluster
// (deploy-kit spec/v1/55-delivery.md#rollouts).
type Deployments struct {
	App
}

// Deployment is a Rollout as GitHub holds it: what the composition workflow created, and the
// payload it named the release with.
type Deployment struct {
	ID      int64
	Project string
	Version string
	Created time.Time
}

// Status is one entry of a Deployment's history.
type Status struct {
	State          string
	Description    string
	LogURL         string
	EnvironmentURL string
}

// Find returns the latest Deployment of commit sha to environment, and false where there is none.
// GitHub lists the newest first.
func (d Deployments) Find(ctx context.Context, repository, sha, environment string) (Deployment, bool, error) {
	token, err := d.token(ctx)
	if err != nil {
		return Deployment{}, false, err
	}
	query := url.Values{"sha": {sha}, "environment": {environment}, "per_page": {"1"}}
	var found []struct {
		ID      int64     `json:"id"`
		Created time.Time `json:"created_at"`
		Payload struct {
			Project string `json:"project"`
			Version string `json:"version"`
		} `json:"payload"`
	}
	target := fmt.Sprintf("%s/repos/%s/deployments?%s", d.root(), repository, query.Encode())
	if err := d.do(ctx, http.MethodGet, target, "Bearer "+token, nil, &found, http.StatusOK); err != nil {
		return Deployment{}, false, fmt.Errorf("find the Deployment of %s@%s: %w", repository, sha, err)
	}
	if len(found) == 0 {
		return Deployment{}, false, nil
	}
	f := found[0]
	return Deployment{ID: f.ID, Project: f.Payload.Project, Version: f.Payload.Version, Created: f.Created}, true, nil
}

// Latest returns the newest status of a Deployment, and false where it has none.
func (d Deployments) Latest(ctx context.Context, repository string, id int64) (Status, bool, error) {
	token, err := d.token(ctx)
	if err != nil {
		return Status{}, false, err
	}
	var statuses []struct {
		State          string `json:"state"`
		Description    string `json:"description"`
		LogURL         string `json:"log_url"`
		EnvironmentURL string `json:"environment_url"`
	}
	if err := d.do(ctx, http.MethodGet, d.statuses(repository, id)+"?per_page=1", "Bearer "+token, nil, &statuses, http.StatusOK); err != nil {
		return Status{}, false, fmt.Errorf("read the status of Deployment %d: %w", id, err)
	}
	if len(statuses) == 0 {
		return Status{}, false, nil
	}
	s := statuses[0]
	return Status{State: s.State, Description: s.Description, LogURL: s.LogURL, EnvironmentURL: s.EnvironmentURL}, true, nil
}

// Post adds a status to a Deployment. A successful one marks every earlier Deployment of the same
// environment inactive, as GitHub does by default.
func (d Deployments) Post(ctx context.Context, repository string, id int64, status Status) error {
	token, err := d.token(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"state":           status.State,
		"description":     status.Description,
		"log_url":         status.LogURL,
		"environment_url": status.EnvironmentURL,
		"auto_inactive":   true,
	})
	if err != nil {
		return err
	}
	if err := d.do(ctx, http.MethodPost, d.statuses(repository, id), "Bearer "+token, body, nil, http.StatusCreated); err != nil {
		return fmt.Errorf("write %s to Deployment %d: %w", status.State, id, err)
	}
	return nil
}

func (d Deployments) statuses(repository string, id int64) string {
	return fmt.Sprintf("%s/repos/%s/deployments/%d/statuses", d.root(), repository, id)
}
