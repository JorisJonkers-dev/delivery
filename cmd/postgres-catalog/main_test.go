package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestARunWithoutItsVaultOrItsCatalogFailsBeforeTouchingAnything(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	malformed := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(malformed, []byte(`{"databases":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
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
