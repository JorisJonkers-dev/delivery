package vaultpolicy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/vaultpolicy"
)

// What the render writes for the postgres Process of the data project
// (deploy-kit spec/v1/examples/_estate/rendered/apps/vso-secrets/policies/).
const (
	postgresPolicy = `{
  "path": {
    "secret/data/platform/postgres/exporter": {
      "capabilities": [
        "read"
      ]
    },
    "secret/metadata/platform/postgres/exporter": {
      "capabilities": [
        "read"
      ]
    }
  }
}
`
	postgresRole = `{
  "bound_service_account_names": [
    "postgres"
  ],
  "bound_service_account_namespaces": [
    "data-system"
  ],
  "token_policies": [
    "data-system-postgres"
  ]
}
`
)

func rendered(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func postgres() map[string]string {
	return map[string]string{"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": postgresRole}
}

// fakeVault is the six calls the job makes, over memory: enough of Vault to count what a run wrote.
type fakeVault struct {
	srv      *httptest.Server
	mu       sync.Mutex
	policies map[string]string
	roles    map[string]vaultpolicy.Role
	writes   []string
	// broken names the request, as `METHOD path`, that Vault refuses.
	broken string
}

func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	f := &fakeVault{policies: map[string]string{}, roles: map[string]vaultpolicy.Role{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVault) vault() vaultpolicy.Vault {
	return vaultpolicy.Vault{Address: f.srv.URL + "/", Token: "policy-admin-token", HTTP: f.srv.Client()}
}

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	if r.Method+" "+path == f.broken {
		http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
		return
	}
	if path == "auth/kubernetes/login" {
		var login map[string]string
		_ = json.NewDecoder(r.Body).Decode(&login)
		if login["role"] != "policy-admin" || login["jwt"] != "sa-token" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `{"auth":{"client_token":"policy-admin-token"}}`)
		return
	}
	if r.Header.Get("X-Vault-Token") != "policy-admin-token" {
		http.Error(w, `{"errors":["missing client token"]}`, http.StatusForbidden)
		return
	}
	keys := func(names []string) {
		if len(names) == 0 {
			http.NotFound(w, r)
			return
		}
		slices.Sort(names)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": names}})
	}
	switch name, isPolicy := strings.CutPrefix(path, "sys/policies/acl/"); {
	case r.Method == "LIST" && path == "sys/policies/acl":
		keys(mapKeys(f.policies))
	case r.Method == "LIST" && path == "auth/kubernetes/role":
		keys(mapKeys(f.roles))
	case isPolicy && r.Method == http.MethodGet:
		policy, held := f.policies[name]
		if !held {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"name": name, "policy": policy}})
	case isPolicy && r.Method == http.MethodPut:
		var sent map[string]string
		_ = json.NewDecoder(r.Body).Decode(&sent)
		f.policies[name] = sent["policy"]
		f.writes = append(f.writes, "policy "+name)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.serveRole(w, r, strings.TrimPrefix(path, "auth/kubernetes/role/"))
	}
}

func (f *fakeVault) serveRole(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		role, held := f.roles[name]
		if !held {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": role})
	case http.MethodPost:
		var role vaultpolicy.Role
		_ = json.NewDecoder(r.Body).Decode(&role)
		f.roles[name] = role
		f.writes = append(f.writes, "role "+name)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected", http.StatusMethodNotAllowed)
	}
}

func mapKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}

func TestTheRenderedDocumentsAreReadByName(t *testing.T) {
	files := postgres()
	files["auth-system-auth-api.policy.json"] = `{"path":{"database/creds/auth":{"capabilities":["read"]},"transit/sign/auth-api-jwt":{"capabilities":["update"]}}}`
	files["auth-system-auth-api.role.json"] = `{"bound_service_account_names":["auth-api"],"bound_service_account_namespaces":["auth-system"],"token_policies":["auth-system-auth-api"]}`
	// What a mounted ConfigMap keeps beside its files.
	files["..data"] = "bookkeeping"

	got, err := vaultpolicy.Read(rendered(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "auth-system-auth-api" || got[1].Name != "data-system-postgres" {
		t.Fatalf("documents = %+v", got)
	}
	// The policy is the encoding of what was checked, whatever the file's own spelling.
	if want := "{\n  \"path\": {\n    \"database/creds/auth\": {\n      \"capabilities\": [\n        \"read\"\n      ]\n    },\n    \"transit/sign/auth-api-jwt\": {\n      \"capabilities\": [\n        \"update\"\n      ]\n    }\n  }\n}\n"; got[0].Policy != want {
		t.Fatalf("auth-api's policy = %q", got[0].Policy)
	}
	want := vaultpolicy.Role{ServiceAccounts: []string{"postgres"}, Namespaces: []string{"data-system"}, Policies: []string{"data-system-postgres"}}
	if got[1].Policy != postgresPolicy || !slices.Equal(got[1].Role.ServiceAccounts, want.ServiceAccounts) ||
		!slices.Equal(got[1].Role.Namespaces, want.Namespaces) || !slices.Equal(got[1].Role.Policies, want.Policies) {
		t.Fatalf("postgres = %+v", got[1])
	}
}

func TestADirectoryThatHoldsAnythingButTheRendersDocumentsIsRefusedWhole(t *testing.T) {
	role := func(sa, namespace, policy string) string {
		return `{"bound_service_account_names":["` + sa + `"],"bound_service_account_namespaces":["` + namespace + `"],"token_policies":["` + policy + `"]}`
	}
	grants := func(path string) string { return `{"path":{"` + path + `":{"capabilities":["read"]}}}` }
	cases := map[string]map[string]string{
		"a policy with no role":          {"data-system-postgres.policy.json": postgresPolicy},
		"a role with no policy":          {"data-system-postgres.role.json": postgresRole},
		"a file that is neither":         {"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": postgresRole, "notes.txt": "x"},
		"a role that is not JSON":        {"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": "{"},
		"a role with a field of its own": {"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": `{"bound_service_account_names":["postgres"],"bound_service_account_namespaces":["data-system"],"token_policies":["data-system-postgres"],"token_ttl":"8760h"}`},
		"a policy that is not JSON":      {"data-system-postgres.policy.json": "path {", "data-system-postgres.role.json": postgresRole},
		"a policy with a field of its own": {
			"data-system-postgres.policy.json": `{"path":{},"name":"x"}`, "data-system-postgres.role.json": postgresRole,
		},
		// The role binds what its name says, and only that.
		"a role for another ServiceAccount": {"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": role("vault", "data-system", "data-system-postgres")},
		"a role for another namespace":      {"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": role("postgres", "secrets-system", "data-system-postgres")},
		"a role for two ServiceAccounts": {
			"data-system-postgres.policy.json": postgresPolicy,
			"data-system-postgres.role.json":   `{"bound_service_account_names":["postgres","vault"],"bound_service_account_namespaces":["data-system"],"token_policies":["data-system-postgres"]}`,
		},
		"a role bound to another policy": {"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": role("postgres", "data-system", "policy-admin")},
		"a role bound to two policies": {
			"data-system-postgres.policy.json": postgresPolicy,
			"data-system-postgres.role.json":   `{"bound_service_account_names":["postgres"],"bound_service_account_namespaces":["data-system"],"token_policies":["data-system-postgres","root"]}`,
		},
		"a role with no ServiceAccount": {"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": `{"bound_service_account_namespaces":["data-system"],"token_policies":["data-system-postgres"]}`},
		// The policy grants only where a grant can derive.
		"a policy on Vault's own paths":  {"data-system-postgres.policy.json": grants("sys/policies/acl/root"), "data-system-postgres.role.json": postgresRole},
		"a policy on the auth method":    {"data-system-postgres.policy.json": grants("auth/kubernetes/role/policy-admin"), "data-system-postgres.role.json": postgresRole},
		"a policy on a whole mount":      {"data-system-postgres.policy.json": grants("secret/data/*"), "data-system-postgres.role.json": postgresRole},
		"a policy with a segment glob":   {"data-system-postgres.policy.json": grants("secret/data/+/token"), "data-system-postgres.role.json": postgresRole},
		"a policy that climbs out":       {"data-system-postgres.policy.json": grants("secret/data/../../sys/raw"), "data-system-postgres.role.json": postgresRole},
		"a policy on the mount's parent": {"data-system-postgres.policy.json": grants("secret/config"), "data-system-postgres.role.json": postgresRole},
		"a policy on the mount itself":   {"data-system-postgres.policy.json": grants("secret/data/"), "data-system-postgres.role.json": postgresRole},
		"a policy on a templated path":   {"data-system-postgres.policy.json": grants("secret/data/{{identity.entity.name}}"), "data-system-postgres.role.json": postgresRole},
		"a policy on a key's own record": {"data-system-postgres.policy.json": grants("transit/keys/auth-api-jwt"), "data-system-postgres.role.json": postgresRole},
		"a policy that exports a key":    {"data-system-postgres.policy.json": grants("transit/export/signing-key/auth-api-jwt"), "data-system-postgres.role.json": postgresRole},
		"a policy on every credential":   {"data-system-postgres.policy.json": grants("database/creds/auth/extra"), "data-system-postgres.role.json": postgresRole},
		// And only what a grant may do there.
		"a policy that grants sudo": {
			"data-system-postgres.policy.json": `{"path":{"secret/data/platform/postgres/exporter":{"capabilities":["read","sudo"]}}}`, "data-system-postgres.role.json": postgresRole,
		},
		"a policy that writes a credential": {
			"data-system-postgres.policy.json": `{"path":{"database/creds/auth":{"capabilities":["update"]}}}`, "data-system-postgres.role.json": postgresRole,
		},
		"a policy that writes a kv document": {
			"data-system-postgres.policy.json": `{"path":{"secret/data/platform/postgres/exporter":{"capabilities":["read","update"]}}}`, "data-system-postgres.role.json": postgresRole,
		},
		"a policy that grants nothing on a path": {
			"data-system-postgres.policy.json": `{"path":{"secret/data/platform/postgres/exporter":{"capabilities":[]}}}`, "data-system-postgres.role.json": postgresRole,
		},
		// One value, and nothing a second parser could read after it.
		"a policy followed by another": {
			"data-system-postgres.policy.json": postgresPolicy + `{"path":{"sys/policies/acl/root":{"capabilities":["sudo"]}}}`, "data-system-postgres.role.json": postgresRole,
		},
		"a role followed by another": {"data-system-postgres.policy.json": postgresPolicy, "data-system-postgres.role.json": postgresRole + postgresRole},
		// A role binds an identity of a Project's namespace, by name alone.
		"a role outside a Project's namespace": {"default-builder.policy.json": postgresPolicy, "default-builder.role.json": role("builder", "default", "default-builder")},
		// A name has one reading: the Project stands before the first `-system-`. A namespace
		// and ServiceAccount that spell the same name another way are another identity.
		"a role that reads its name another way": {
			"data-system-api-system-worker.policy.json": postgresPolicy,
			"data-system-api-system-worker.role.json":   role("worker", "data-system-api-system", "data-system-api-system-worker"),
		},
		"a name with no identity": {"data-system-.policy.json": postgresPolicy, "data-system-.role.json": role("", "data-system", "data-system-")},
		"a name with no Project":  {"-system-postgres.policy.json": postgresPolicy, "-system-postgres.role.json": role("postgres", "-system", "-system-postgres")},
		"a role with a namespace selector": {
			"data-system-postgres.policy.json": postgresPolicy,
			"data-system-postgres.role.json":   `{"bound_service_account_names":["postgres"],"bound_service_account_namespaces":["data-system"],"token_policies":["data-system-postgres"],"bound_service_account_namespace_selector":"{\"matchLabels\":{}}"}`,
		},
		// A name becomes a path in Vault's API.
		"a name that is a path": {"Data_System.policy.json": postgresPolicy, "Data_System.role.json": postgresRole},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := vaultpolicy.Read(rendered(t, files)); err == nil {
				t.Fatalf("read as %+v", got)
			}
		})
	}
	if _, err := vaultpolicy.Read(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a directory that is not there is no render")
	}
	// A directory among the files cannot be read as one.
	dir := rendered(t, postgres())
	if err := os.Mkdir(filepath.Join(dir, "more.policy.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := vaultpolicy.Read(dir); err == nil {
		t.Fatal("a directory named like a policy is not one")
	}
}

func TestARunWritesWhatVaultDoesNotHoldAsRenderedAndLeavesTheRest(t *testing.T) {
	f := newFakeVault(t)
	documents, err := vaultpolicy.Read(rendered(t, postgres()))
	if err != nil {
		t.Fatal(err)
	}

	first, err := vaultpolicy.Apply(t.Context(), f.vault(), documents)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Written, []string{"policy data-system-postgres", "role data-system-postgres"}) || first.Unchanged != 0 || len(first.Stale) != 0 {
		t.Fatalf("first run = %+v", first)
	}
	if f.policies["data-system-postgres"] != postgresPolicy || !slices.Equal(f.roles["data-system-postgres"].Policies, []string{"data-system-postgres"}) {
		t.Fatalf("Vault holds %v %v", f.policies, f.roles)
	}

	// The same render again writes nothing.
	f.writes = nil
	second, err := vaultpolicy.Apply(t.Context(), f.vault(), documents)
	if err != nil || len(second.Written) != 0 || second.Unchanged != 2 || len(f.writes) != 0 {
		t.Fatalf("second run = %+v, %v, wrote %v", second, err, f.writes)
	}

	// A policy someone changed by hand, and a role bound elsewhere, are put back.
	f.policies["data-system-postgres"] = `{"path":{}}`
	drifted := f.roles["data-system-postgres"]
	drifted.Namespaces = []string{"elsewhere"}
	f.roles["data-system-postgres"] = drifted
	third, err := vaultpolicy.Apply(t.Context(), f.vault(), documents)
	if err != nil || !slices.Equal(third.Written, []string{"policy data-system-postgres", "role data-system-postgres"}) {
		t.Fatalf("third run = %+v, %v", third, err)
	}
	for field, change := range map[string]func(*vaultpolicy.Role){
		"service accounts": func(r *vaultpolicy.Role) { r.ServiceAccounts = []string{"other"} },
		"policies":         func(r *vaultpolicy.Role) { r.Policies = []string{"other"} },
		// A selector admits more namespaces than the one named, so a role that gained one is not as rendered.
		"namespace selector": func(r *vaultpolicy.Role) { r.NamespaceSelector = `{"matchLabels":{"team":"any"}}` },
	} {
		role := f.roles["data-system-postgres"]
		change(&role)
		f.roles["data-system-postgres"] = role
		again, err := vaultpolicy.Apply(t.Context(), f.vault(), documents)
		if err != nil || !slices.Equal(again.Written, []string{"role data-system-postgres"}) {
			t.Fatalf("a role whose %s drifted = %+v, %v", field, again, err)
		}
	}
}

func TestWhatTheRenderNoLongerNamesIsReportedAndNeverDeleted(t *testing.T) {
	f := newFakeVault(t)
	// What an earlier render wrote, and what the platform holds for itself.
	f.policies["notes-system-notes-api"] = `{"path":{}}`
	f.roles["notes-system-notes-api"] = vaultpolicy.Role{}
	f.roles["old-system-worker"] = vaultpolicy.Role{}
	for _, fixture := range []string{"default", "root", "policy-admin", "-system-x", "x-system-"} {
		f.policies[fixture] = `{"path":{}}`
		f.roles[fixture] = vaultpolicy.Role{}
	}
	documents, err := vaultpolicy.Read(rendered(t, postgres()))
	if err != nil {
		t.Fatal(err)
	}

	report, err := vaultpolicy.Apply(t.Context(), f.vault(), documents)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"policy notes-system-notes-api", "role notes-system-notes-api", "role old-system-worker"}
	if !slices.Equal(report.Stale, want) {
		t.Fatalf("stale = %v, want %v", report.Stale, want)
	}
	if _, held := f.policies["notes-system-notes-api"]; !held || len(f.roles) != 8 {
		t.Fatalf("the job deleted something: %v %v", mapKeys(f.policies), mapKeys(f.roles))
	}

	// An empty Vault lists nothing, and an empty render writes nothing.
	empty := newFakeVault(t)
	if report, err := vaultpolicy.Apply(t.Context(), empty.vault(), nil); err != nil || len(report.Written)+len(report.Stale)+report.Unchanged != 0 {
		t.Fatalf("nothing over nothing = %+v, %v", report, err)
	}
}

func TestACallVaultRefusesStopsTheRun(t *testing.T) {
	documents, err := vaultpolicy.Read(rendered(t, postgres()))
	if err != nil {
		t.Fatal(err)
	}
	for _, broken := range []string{
		"GET sys/policies/acl/data-system-postgres", "PUT sys/policies/acl/data-system-postgres",
		"GET auth/kubernetes/role/data-system-postgres", "POST auth/kubernetes/role/data-system-postgres",
		"LIST sys/policies/acl", "LIST auth/kubernetes/role",
	} {
		t.Run(broken, func(t *testing.T) {
			f := newFakeVault(t)
			f.broken = broken
			if _, err := vaultpolicy.Apply(t.Context(), f.vault(), documents); err == nil || !strings.Contains(err.Error(), "403") {
				t.Fatalf("apply with %s refused = %v", broken, err)
			}
		})
	}
	// A Vault that is not there, and one that answers with something that is not JSON.
	gone := newFakeVault(t)
	gone.srv.Close()
	if _, err := vaultpolicy.Apply(t.Context(), gone.vault(), documents); err == nil {
		t.Fatal("a Vault that is away is an error")
	}
	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>") }))
	defer garbled.Close()
	if _, err := vaultpolicy.Apply(t.Context(), vaultpolicy.Vault{Address: garbled.URL, HTTP: garbled.Client()}, documents); err == nil {
		t.Fatal("an answer that is not Vault's is an error")
	}
	if _, err := vaultpolicy.Apply(t.Context(), vaultpolicy.Vault{Address: "http://[::1", HTTP: http.DefaultClient}, documents); err == nil {
		t.Fatal("an address that is not one is an error")
	}
}

func TestTheJobLogsInAsItsOwnRole(t *testing.T) {
	f := newFakeVault(t)
	vault, err := vaultpolicy.Login(t.Context(), f.srv.Client(), f.srv.URL, "policy-admin", "sa-token")
	if err != nil || vault.Token != "policy-admin-token" {
		t.Fatalf("login = %+v, %v", vault, err)
	}
	if _, err := vaultpolicy.Login(t.Context(), f.srv.Client(), f.srv.URL, "someone-else", "sa-token"); err == nil {
		t.Fatal("a role Vault does not let this ServiceAccount take is no login")
	}
	// A Vault with no such auth method, and one that answers with no token.
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	if _, err := vaultpolicy.Login(t.Context(), missing.Client(), missing.URL, "policy-admin", "sa-token"); err == nil {
		t.Fatal("no auth method, no login")
	}
	tokenless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"auth":{}}`) }))
	defer tokenless.Close()
	if _, err := vaultpolicy.Login(t.Context(), tokenless.Client(), tokenless.URL, "policy-admin", "sa-token"); err == nil {
		t.Fatal("no token, no login")
	}
}
