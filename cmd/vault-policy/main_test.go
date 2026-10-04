package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	policy = `{"path":{"secret/data/notes/token":{"capabilities":["read"]}}}`
	role   = `{"bound_service_account_names":["notes-api"],"bound_service_account_namespaces":["notes-system"],"token_policies":["notes-system-notes-api"]}`
)

// vault answers the job's calls: a login as policy-admin, an empty Vault but for one stale
// policy, and every write accepted.
func vault(t *testing.T, writes *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case path == "auth/kubernetes/login":
			var login map[string]string
			_ = json.NewDecoder(r.Body).Decode(&login)
			if login["role"] != "policy-admin" || login["jwt"] != "sa-token" {
				http.Error(w, "permission denied", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, `{"auth":{"client_token":"t"}}`)
		case r.Method == "LIST" && path == "sys/policies/acl":
			_, _ = io.WriteString(w, `{"data":{"keys":["default","old-system-worker"]}}`)
		case r.Method == http.MethodGet || r.Method == "LIST":
			http.NotFound(w, r)
		default:
			*writes = append(*writes, r.Method+" "+path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func render(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{"notes-system-notes-api.policy.json": policy, "notes-system-notes-api.role.json": role} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func token(string) ([]byte, error) { return []byte("sa-token\n"), nil }

func TestARunWritesTheRenderAndReportsWhatItNoLongerNames(t *testing.T) {
	var writes []string
	srv := vault(t, &writes)
	env := map[string]string{"VAULT_ADDR": srv.URL, "POLICIES_DIR": render(t)}
	var logged bytes.Buffer

	code := start(t.Context(), slog.New(slog.NewJSONHandler(&logged, nil)), func(k string) string { return env[k] }, token)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, logged.String())
	}
	if strings.Join(writes, ", ") != "PUT sys/policies/acl/notes-system-notes-api, POST auth/kubernetes/role/notes-system-notes-api" {
		t.Fatalf("wrote %v", writes)
	}
	for _, want := range []string{`"written":["policy notes-system-notes-api","role notes-system-notes-api"]`, `"stale":["policy old-system-worker"]`, `"level":"WARN"`} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("the log lacks %s: %s", want, logged.String())
		}
	}
}

func TestARunThatCannotApplyTheRenderExitsOneAndWritesNothing(t *testing.T) {
	cases := map[string]struct {
		change   func(env map[string]string, dir string)
		readFile func(string) ([]byte, error)
		want     string
	}{
		"no Vault address": {func(env map[string]string, _ string) { delete(env, "VAULT_ADDR") }, token, "VAULT_ADDR is not set"},
		"no render":        {func(env map[string]string, _ string) { env["POLICIES_DIR"] = "/nowhere/at/all" }, token, "read /nowhere/at/all"},
		"a render that holds something else": {func(_ map[string]string, dir string) {
			_ = os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o600)
		}, token, "neither a policy nor a role"},
		"no ServiceAccount token": {func(map[string]string, string) {}, func(string) ([]byte, error) { return nil, errors.New("no such file") }, "read the ServiceAccount token"},
		"a role Vault refuses":    {func(env map[string]string, _ string) { env["VAULT_ROLE"] = "someone-else" }, token, "log in as someone-else"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var writes []string
			srv := vault(t, &writes)
			dir := render(t)
			env := map[string]string{"VAULT_ADDR": srv.URL, "POLICIES_DIR": dir}
			c.change(env, dir)
			var logged bytes.Buffer

			code := start(t.Context(), slog.New(slog.NewJSONHandler(&logged, nil)), func(k string) string { return env[k] }, c.readFile)

			if code != 1 || !strings.Contains(logged.String(), c.want) || len(writes) != 0 {
				t.Fatalf("exit %d, wrote %v, log %s", code, writes, logged.String())
			}
		})
	}
}

func TestTheTokenIsReadFromThePodsOwnFileUnlessToldOtherwise(t *testing.T) {
	var writes []string
	srv := vault(t, &writes)
	var asked []string
	reading := func(path string) ([]byte, error) {
		asked = append(asked, path)
		return []byte("sa-token"), nil
	}
	env := map[string]string{"VAULT_ADDR": srv.URL, "POLICIES_DIR": render(t)}
	if code := start(t.Context(), slog.New(slog.DiscardHandler), func(k string) string { return env[k] }, reading); code != 0 {
		t.Fatalf("exit %d", code)
	}
	env["TOKEN_FILE"] = "/elsewhere/token"
	if code := start(t.Context(), slog.New(slog.DiscardHandler), func(k string) string { return env[k] }, reading); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Join(asked, " ") != "/var/run/secrets/kubernetes.io/serviceaccount/token /elsewhere/token" {
		t.Fatalf("token read from %v", asked)
	}
}
