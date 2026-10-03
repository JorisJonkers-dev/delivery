package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/JorisJonkers-dev/delivery/internal/githubapp/githubtest"
)

func environment(github *githubtest.Server) map[string]string {
	return map[string]string{
		"CLUSTER_NAME":                "production",
		"ESTATE_REPOSITORY":           githubtest.Repository,
		"GITHUB_APP_ID":               githubtest.AppID,
		"GITHUB_APP_INSTALLATION_ID":  githubtest.InstallationID,
		"GITHUB_APP_PRIVATE_KEY_FILE": "/vault/secrets/private-key",
		"GITHUB_API_URL":              github.URL,
	}
}

func TestStartSaysWhatItLacksAndExitsOne(t *testing.T) {
	github := githubtest.New(t, "")
	readKey := func(string) ([]byte, error) { return github.PEM(), nil }
	unreachable := &rest.Config{Host: "http://127.0.0.1:1"}

	cases := map[string]struct {
		change   func(map[string]string)
		readFile func(string) ([]byte, error)
		config   *rest.Config
		want     string
	}{
		"variables not set": {
			func(env map[string]string) { delete(env, "CLUSTER_NAME"); delete(env, "GITHUB_APP_ID") },
			readKey, unreachable, "not set: CLUSTER_NAME, GITHUB_APP_ID",
		},
		"no key file": {
			func(map[string]string) {},
			func(string) ([]byte, error) { return nil, errors.New("no such file") },
			unreachable, "read the App key: no such file",
		},
		"a key that is not one": {
			func(map[string]string) {},
			func(string) ([]byte, error) { return []byte("not a key"), nil },
			unreachable, "the App key is not PEM",
		},
		"a cluster configuration that is not one": {
			func(map[string]string) {}, readKey,
			&rest.Config{Host: "http://127.0.0.1:1", QPS: 1, Burst: -1, RateLimiter: nil, ExecProvider: nil, AuthProvider: nil, TLSClientConfig: rest.TLSClientConfig{CertFile: "/nowhere/cert", KeyFile: "/nowhere/key"}},
			"reach the cluster",
		},
		"a cluster it cannot reach": {
			func(map[string]string) {}, readKey, unreachable, "list PersistentVolumes",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := environment(github)
			c.change(env)
			var logged bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logged, nil))
			code := start(context.Background(), logger, func(k string) string { return env[k] }, c.readFile, c.config)
			if code != 1 || !strings.Contains(logged.String(), c.want) {
				t.Fatalf("exit %d, log %s, want it to name %q", code, logged.String(), c.want)
			}
			if len(github.Commits()) != 0 {
				t.Fatal("something was committed")
			}
		})
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "main") != "main" || orDefault("release", "main") != "release" {
		t.Fatal("orDefault")
	}
}
