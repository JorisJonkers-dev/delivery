package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/bucketcatalog"
	"github.com/JorisJonkers-dev/delivery/internal/offcluster/garagetest"
	"github.com/JorisJonkers-dev/delivery/internal/vaultpolicy"
)

const catalog = `{"admin":{"path":"secret/data/platform/garage","token":"admin-token"},"host":"garage.data-system.svc.cluster.local","keys":[{"buckets":["knowledge-platform-assets"],"name":"knowledge-platform","path":"secret/data/knowledge-platform/garage/s3"}],"port":3903}`

func file(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestARunWithoutItsVaultOrItsCatalogFailsBeforeTouchingAnything(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	malformed := file(t, `{"keys":[]}`)
	readFile := func(string) ([]byte, error) {
		t.Fatal("the token was read")
		return nil, nil
	}
	for name, env := range map[string]map[string]string{
		"no Vault address":    {"VAULT_ROLE": "r", "CATALOG_FILE": malformed},
		"no Vault role":       {"VAULT_ADDR": "http://vault:8200", "CATALOG_FILE": malformed},
		"a malformed catalog": {"VAULT_ADDR": "http://vault:8200", "VAULT_ROLE": "r", "CATALOG_FILE": malformed},
	} {
		if code := start(t.Context(), logger, func(key string) string { return env[key] }, readFile); code != 1 {
			t.Errorf("%s exited %d", name, code)
		}
	}
}

func TestARunWithNoServiceAccountTokenFails(t *testing.T) {
	env := map[string]string{"VAULT_ADDR": "http://vault:8200", "VAULT_ROLE": "r", "CATALOG_FILE": file(t, catalog)}
	_, err := run(t.Context(), func(key string) string { return env[key] }, func(string) ([]byte, error) {
		return nil, errors.New("no such file")
	})
	if err == nil {
		t.Fatal("ran")
	}
}

// vault answers a login and one KV document, as a Vault holding the admin token would.
func vault(t *testing.T, document map[string]any) vaultpolicy.Vault {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/secret/data/platform/garage":
			if document == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": document}})
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(server.Close)
	return vaultpolicy.Vault{Address: server.URL, Token: "t", HTTP: server.Client()}
}

func TestTheAdminTokenIsReadWhereTheCatalogSaysAndMustBeThere(t *testing.T) {
	admin := bucketcatalog.Admin{Path: "secret/data/platform/garage", Token: "admin-token"}
	if token, err := adminToken(t.Context(), vault(t, map[string]any{"admin-token": "s3cr3t"}), admin); err != nil || token != "s3cr3t" {
		t.Fatalf("token %q, %v", token, err)
	}
	for name, document := range map[string]map[string]any{
		"no document":      nil,
		"no such key":      {"rpc-secret": "x"},
		"an empty token":   {"admin-token": ""},
		"a token not text": {"admin-token": 7},
	} {
		if _, err := adminToken(t.Context(), vault(t, document), admin); err == nil {
			t.Errorf("%s: read a token", name)
		}
	}
	if _, err := adminToken(t.Context(), vault(t, nil), bucketcatalog.Admin{Path: "secret/data/elsewhere", Token: "t"}); err == nil {
		t.Error("a path Vault refuses gave a token")
	}
}

// TestARunAppliesTheCatalogAgainstARealGarage drives the command end to end: it logs in to a
// Vault that holds the admin token and no key, applies the catalog to a real Garage, and writes
// the minted key where the catalog says.
func TestARunAppliesTheCatalogAgainstARealGarage(t *testing.T) {
	_, admin := garagetest.StartAdmin(t)
	endpoint, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	written := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/auth/kubernetes/login":
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "t"}})
		case r.URL.Path == "/v1/secret/data/platform/garage":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"admin-token": garagetest.AdminToken}}})
		case r.URL.Path == "/v1/secret/data/knowledge-platform/garage/s3" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&written)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	body := strings.Replace(strings.Replace(catalog, "garage.data-system.svc.cluster.local", endpoint.Hostname(), 1), `"port":3903`, `"port":`+endpoint.Port(), 1)
	env := map[string]string{"VAULT_ADDR": server.URL, "VAULT_ROLE": "data-system-garage-catalog", "CATALOG_FILE": file(t, body), "TOKEN_FILE": "jwt"}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if code := start(t.Context(), logger, func(key string) string { return env[key] }, func(string) ([]byte, error) { return []byte("jwt\n"), nil }); code != 0 {
		t.Fatalf("exited %d", code)
	}
	data, _ := written["data"].(map[string]any)
	options, _ := written["options"].(map[string]any)
	if id, _ := data["access-key-id"].(string); !strings.HasPrefix(id, "GK") || data["secret-access-key"] == "" || options["cas"] != float64(0) {
		t.Fatalf("wrote %v", written)
	}
}
