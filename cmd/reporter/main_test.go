package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/JorisJonkers-dev/delivery/internal/githubapp/githubtest"
)

func environment(github *githubtest.Server) map[string]string {
	return map[string]string{
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
	badTLS := &rest.Config{Host: "http://127.0.0.1:1", TLSClientConfig: rest.TLSClientConfig{CertFile: "/nowhere/cert", KeyFile: "/nowhere/key"}}

	cases := map[string]struct {
		change   func(map[string]string)
		readFile func(string) ([]byte, error)
		config   *rest.Config
		want     string
	}{
		"variables not set": {
			func(env map[string]string) { delete(env, "GITHUB_APP_ID"); delete(env, "GITHUB_APP_INSTALLATION_ID") },
			readKey, unreachable, "not set: GITHUB_APP_ID, GITHUB_APP_INSTALLATION_ID",
		},
		"no key at all": {
			func(env map[string]string) { delete(env, "GITHUB_APP_PRIVATE_KEY_FILE") },
			readKey, unreachable, "not set: GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE",
		},
		"a key in a variable that is not one": {
			func(env map[string]string) { env["GITHUB_APP_PRIVATE_KEY"] = "not a key" },
			func(string) ([]byte, error) { return nil, errors.New("the file is not read when the variable is set") },
			unreachable, "the App key is not PEM",
		},
		"no key file": {
			func(map[string]string) {},
			func(string) ([]byte, error) { return nil, errors.New("no such file") },
			unreachable, "read the App key: no such file",
		},
		"a cluster configuration that is not one": {
			func(map[string]string) {}, readKey, badTLS, "reach the cluster",
		},
		"an interval that is not one": {
			func(env map[string]string) { env["INTERVAL"] = "often" }, readKey, unreachable, `INTERVAL \"often\" is not a duration above zero`,
		},
		"an address it cannot listen on": {
			func(env map[string]string) { env["ADDR"] = "256.0.0.1:1" }, readKey, unreachable, "reporter could not listen",
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
		})
	}
}

// stopsOn is a log that cancels a context when a record carries a message: how a test stops a
// Reporter at a moment it can name, with no clock.
type stopsOn struct {
	slog.Handler
	message string
	stop    context.CancelFunc
}

func (h stopsOn) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if record.Message == h.message {
		h.stop()
	}
	return err
}

func TestAPassThatFailsIsLoggedAndTheReporterKeepsRunningUntilItIsStopped(t *testing.T) {
	github := githubtest.New(t, "")
	env := environment(github)
	env["ADDR"] = "127.0.0.1:0"
	// The key as a variable, which is how an `env` grant delivers it.
	delete(env, "GITHUB_APP_PRIVATE_KEY_FILE")
	env["GITHUB_APP_PRIVATE_KEY"] = string(github.PEM())
	ctx, stop := context.WithCancel(t.Context())
	var logged bytes.Buffer
	logger := slog.New(stopsOn{slog.NewJSONHandler(&logged, nil), "a pass failed", stop})

	code := start(ctx, logger, func(k string) string { return env[k] }, func(string) ([]byte, error) { return nil, errors.New("no file") },
		&rest.Config{Host: "http://127.0.0.1:1"})
	// A cluster it cannot reach fails the pass and not the Reporter: it stopped because it was told to.
	if code != 0 || !strings.Contains(logged.String(), "read the units") {
		t.Fatalf("exit %d, log %s", code, logged.String())
	}
}

func TestTheIntervalIsThirtySecondsUnlessItIsSet(t *testing.T) {
	if got, err := intervalOf(""); err != nil || got != 30*time.Second {
		t.Fatalf("unset = %s, %v", got, err)
	}
	if got, err := intervalOf("1m"); err != nil || got != time.Minute {
		t.Fatalf("1m = %s, %v", got, err)
	}
	if _, err := intervalOf("-1s"); err == nil {
		t.Fatal("a negative interval was taken")
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "flux-system") != "flux-system" || orDefault("flux", "flux-system") != "flux" {
		t.Fatal("orDefault")
	}
}
