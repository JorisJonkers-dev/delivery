package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/bucketcatalog"
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
