// Command garage-catalog applies the bucket catalog the render hands it
// (deploy-kit spec/v1/16-dependencies.md#the-bucket-catalog): each project's key, minted in
// Garage or imported from the Secret Store, and its buckets and grants, then exits.
//
// Environment: VAULT_ADDR and VAULT_ROLE (required), CATALOG_FILE (default
// /catalog/catalog.json) and TOKEN_FILE (default the pod's own ServiceAccount token).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/bucketcatalog"
	"github.com/JorisJonkers-dev/delivery/internal/vaultpolicy"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const (
	defaultCatalog = "/catalog/catalog.json"
	//nolint:gosec // G101: a path, not a credential
	defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// timeout bounds each call to Vault and to Garage.
	timeout = 30 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	os.Exit(start(context.Background(), logger, os.Getenv, os.ReadFile))
}

// start is main minus the process it runs in, so tests can drive it.
func start(parent context.Context, logger *slog.Logger, getenv func(string) string, readFile func(string) ([]byte, error)) int {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := run(ctx, getenv, readFile)
	if err != nil {
		logger.Error("garage-catalog failed", "error", err, "version", version)
		return 1
	}
	logger.Info("garage-catalog applied the catalog", "minted", report.Minted, "imported", report.Imported, "buckets", report.Buckets, "version", version)
	return 0
}

var errUnset = errors.New("is not set")

func run(ctx context.Context, getenv func(string) string, readFile func(string) ([]byte, error)) (bucketcatalog.Report, error) {
	or := func(name, fallback string) string {
		if value := getenv(name); value != "" {
			return value
		}
		return fallback
	}
	address, role := getenv("VAULT_ADDR"), getenv("VAULT_ROLE")
	if address == "" || role == "" {
		return bucketcatalog.Report{}, fmt.Errorf("VAULT_ADDR and VAULT_ROLE: %w", errUnset)
	}
	// The catalog is read whole before anything is touched: half a catalog is not applied.
	c, err := bucketcatalog.Read(or("CATALOG_FILE", defaultCatalog))
	if err != nil {
		return bucketcatalog.Report{}, err
	}
	jwt, err := readFile(or("TOKEN_FILE", defaultTokenFile))
	if err != nil {
		return bucketcatalog.Report{}, errors.New("read the ServiceAccount token: " + err.Error())
	}
	client := &http.Client{Timeout: timeout}
	vault, err := vaultpolicy.Login(ctx, client, address, role, strings.TrimSpace(string(jwt)))
	if err != nil {
		return bucketcatalog.Report{}, err
	}
	token, err := adminToken(ctx, vault, c.Admin)
	if err != nil {
		return bucketcatalog.Report{}, err
	}
	garage := bucketcatalog.Garage{Address: "http://" + c.Host + ":" + strconv.Itoa(c.Port), Token: token, HTTP: client}
	return bucketcatalog.Apply(ctx, garage, vault, c)
}

var errNoToken = errors.New("holds no token")

// adminToken reads the admin API's token from the Secret Store, where the catalog says it is.
func adminToken(ctx context.Context, vault vaultpolicy.Vault, admin bucketcatalog.Admin) (string, error) {
	held, err := vault.ReadKV(ctx, admin.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", admin.Path, err)
	}
	var token string
	if json.Unmarshal(held[admin.Token], &token) != nil || token == "" {
		return "", fmt.Errorf("%s#%s %w", admin.Path, admin.Token, errNoToken)
	}
	return token, nil
}
