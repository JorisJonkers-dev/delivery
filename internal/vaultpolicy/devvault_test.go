package vaultpolicy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JorisJonkers-dev/delivery/internal/vaultpolicy"
)

const (
	// A released Vault, pinned by tag: the job is held to the API a real one serves.
	vaultImage = "hashicorp/vault:1.20.4"
	rootToken  = "dev-root-token" //nolint:gosec // G101: the root token of a Vault this test starts and discards
)

// devVault starts a Vault in dev mode with the Kubernetes auth method mounted, as the platform
// mounts it, and returns it as its root token. It skips under -short, which runs without Docker.
func devVault(t *testing.T) vaultpolicy.Vault {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Docker; skipped with -short")
	}
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        vaultImage,
			ExposedPorts: []string{"8200/tcp"},
			Env:          map[string]string{"VAULT_DEV_ROOT_TOKEN_ID": rootToken, "VAULT_DEV_LISTEN_ADDRESS": "0.0.0.0:8200"},
			WaitingFor:   wait.ForHTTP("/v1/sys/health").WithPort("8200/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start Vault: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	endpoint, err := container.PortEndpoint(ctx, "8200/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	vault := vaultpolicy.Vault{Address: endpoint, Token: rootToken, HTTP: &http.Client{Timeout: 10 * time.Second}}
	admin(t, vault, http.MethodPost, "sys/auth/kubernetes", `{"type":"kubernetes"}`)
	return vault
}

// admin makes one call as the root token, for what the platform sets up and the job does not.
func admin(t *testing.T, vault vaultpolicy.Vault, method, path, body string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, vault.Address+"/v1/"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Vault-Token", vault.Token)
	res, err := vault.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= http.StatusMultipleChoices {
		t.Fatalf("%s %s = %d %s", method, path, res.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func strings2(values any) []string {
	list, _ := values.([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		text, _ := v.(string)
		out = append(out, text)
	}
	return out
}

func TestAgainstADevVaultThePoliciesAndRolesMatchTheRender(t *testing.T) {
	vault := devVault(t)
	files := postgres()
	files["auth-system-auth-api.policy.json"] = `{"path":{"database/creds/auth":{"capabilities":["read"]},"transit/sign/auth-api-jwt":{"capabilities":["update"]}}}`
	files["auth-system-auth-api.role.json"] = `{"bound_service_account_names":["auth-api"],"bound_service_account_namespaces":["auth-system"],"token_policies":["auth-system-auth-api"]}`
	documents, err := vaultpolicy.Read(rendered(t, files))
	if err != nil {
		t.Fatal(err)
	}
	// What an earlier render wrote for an identity this one no longer names.
	admin(t, vault, http.MethodPut, "sys/policies/acl/notes-system-notes-api", `{"policy":"{\"path\":{}}"}`)
	admin(t, vault, http.MethodPost, "auth/kubernetes/role/notes-system-notes-api",
		`{"bound_service_account_names":["notes-api"],"bound_service_account_namespaces":["notes-system"],"token_policies":["notes-system-notes-api"]}`)

	first, err := vaultpolicy.Apply(t.Context(), vault, documents)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"policy auth-system-auth-api", "role auth-system-auth-api", "policy data-system-postgres", "role data-system-postgres"}
	if !slices.Equal(first.Written, want) {
		t.Fatalf("written = %v, want %v", first.Written, want)
	}

	// Vault holds each policy as rendered, and each role bound as rendered.
	held := admin(t, vault, http.MethodGet, "sys/policies/acl/data-system-postgres", "")
	if data, _ := held["data"].(map[string]any); data["policy"] != postgresPolicy {
		t.Fatalf("the policy Vault holds = %v", data["policy"])
	}
	role := admin(t, vault, http.MethodGet, "auth/kubernetes/role/data-system-postgres", "")
	data, _ := role["data"].(map[string]any)
	if !slices.Equal(strings2(data["bound_service_account_names"]), []string{"postgres"}) ||
		!slices.Equal(strings2(data["bound_service_account_namespaces"]), []string{"data-system"}) ||
		!slices.Equal(strings2(data["token_policies"]), []string{"data-system-postgres"}) {
		t.Fatalf("the role Vault holds = %v", data)
	}
	// Vault parses the policy the way the render means it: the path is granted, and no other.
	capabilities := admin(t, vault, http.MethodPost, "sys/capabilities",
		`{"token":"`+tokenFor(t, vault, "data-system-postgres")+`","paths":["secret/data/platform/postgres/exporter","secret/data/platform/postgres/auth"]}`)
	granted, _ := capabilities["data"].(map[string]any)
	if !slices.Equal(strings2(granted["secret/data/platform/postgres/exporter"]), []string{"read"}) ||
		!slices.Equal(strings2(granted["secret/data/platform/postgres/auth"]), []string{"deny"}) {
		t.Fatalf("what the policy grants = %v", granted)
	}

	// A second run of the same render writes nothing, and what the render no longer names is
	// reported and still there.
	second, err := vaultpolicy.Apply(t.Context(), vault, documents)
	if err != nil || len(second.Written) != 0 || second.Unchanged != 4 {
		t.Fatalf("second run = %+v, %v", second, err)
	}
	if !slices.Equal(second.Stale, []string{"policy notes-system-notes-api", "role notes-system-notes-api"}) {
		t.Fatalf("stale = %v", second.Stale)
	}
	admin(t, vault, http.MethodGet, "sys/policies/acl/notes-system-notes-api", "")
	admin(t, vault, http.MethodGet, "auth/kubernetes/role/notes-system-notes-api", "")
}

// tokenFor mints a token holding one policy, to ask Vault what that policy grants.
func tokenFor(t *testing.T, vault vaultpolicy.Vault, policy string) string {
	t.Helper()
	minted := admin(t, vault, http.MethodPost, "auth/token/create", `{"policies":["`+policy+`"],"no_default_policy":true,"ttl":"1m"}`)
	auth, _ := minted["auth"].(map[string]any)
	token, _ := auth["client_token"].(string)
	if token == "" {
		t.Fatalf("no token minted: %v", minted)
	}
	return token
}
