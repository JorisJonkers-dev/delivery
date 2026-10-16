package catalog_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JorisJonkers-dev/delivery/internal/catalog"
	"github.com/JorisJonkers-dev/delivery/internal/vaultpolicy"
)

const (
	vaultImage = "hashicorp/vault:1.20.4"
	rootToken  = "dev-root-token" //nolint:gosec // G101: the root token of a Vault this test starts and discards
)

// call makes one request as the root token and decodes the answer.
func call(t *testing.T, vault vaultpolicy.Vault, method, path, body string) map[string]any {
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

func TestTheDatabaseRolesAreWhatVaultsDatabaseEngineAccepts(t *testing.T) {
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
	// The platform's fixture: the engine and its connection, admitting the roles the catalog writes.
	call(t, vault, http.MethodPost, "sys/mounts/database", `{"type":"database"}`)
	call(t, vault, http.MethodPost, "database/config/postgres", `{"plugin_name":"postgresql-database-plugin",`+
		`"connection_url":"postgresql://{{username}}:{{password}}@nowhere:5432/postgres","username":"x","password":"y",`+
		`"verify_connection":false,"allowed_roles":"*-owner,*-data"}`)

	c := catalog.Catalog{Connection: "postgres", Databases: []catalog.Database{
		{Name: "tribelt_db", Owner: "tribelt-owner", Data: "tribelt-data"},
	}}
	written, err := catalog.WriteRoles(t.Context(), vault, c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(written, " ") != "database/roles/tribelt-owner database/roles/tribelt-data" {
		t.Fatalf("written = %v", written)
	}
	role := call(t, vault, http.MethodGet, "database/roles/tribelt-data", "")["data"].(map[string]any)
	statements := role["creation_statements"].([]any)
	if role["db_name"] != "postgres" || len(statements) != 2 ||
		!strings.Contains(statements[0].(string), `IN ROLE "tribelt-data" INHERIT;`) ||
		statements[1] != `ALTER ROLE "{{name}}" SET role = "tribelt-data";` {
		t.Fatalf("role = %v", role)
	}

	// Written again, the same roles: a run is safe to repeat.
	if _, err := catalog.WriteRoles(t.Context(), vault, c); err != nil {
		t.Fatal(err)
	}
}
