// Command postgres-catalog applies the database catalog the render hands it
// (deploy-kit spec/v1/16-dependencies.md#applying-the-catalog): each project's
// database and roles in Postgres, then the Secret Store's database roles that
// issue their logins, and exits. It runs as a Job of the `estate-vso-secrets`
// Reconcile Unit, so no Application that reads a database starts before it.
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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JorisJonkers-dev/delivery/internal/catalog"
	"github.com/JorisJonkers-dev/delivery/internal/vaultpolicy"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const (
	defaultCatalog = "/catalog/catalog.json"
	//nolint:gosec // G101: a path, not a credential
	defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// timeout bounds each call to Vault and each connection to Postgres.
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
	report, roles, err := run(ctx, getenv, readFile)
	if err != nil {
		logger.Error("postgres-catalog failed", "error", err, "version", version)
		return 1
	}
	logger.Info("postgres-catalog applied the catalog", "created", report.Created, "adopted", report.Adopted, "roles", roles, "version", version)
	return 0
}

var errUnset = errors.New("is not set")

func run(ctx context.Context, getenv func(string) string, readFile func(string) ([]byte, error)) (catalog.Report, []string, error) {
	or := func(name, fallback string) string {
		if value := getenv(name); value != "" {
			return value
		}
		return fallback
	}
	address, role := getenv("VAULT_ADDR"), getenv("VAULT_ROLE")
	if address == "" || role == "" {
		return catalog.Report{}, nil, fmt.Errorf("VAULT_ADDR and VAULT_ROLE: %w", errUnset)
	}
	// The catalog is read whole before anything is touched: half a catalog is not applied.
	c, err := catalog.Read(or("CATALOG_FILE", defaultCatalog))
	if err != nil {
		return catalog.Report{}, nil, err
	}
	token, err := readFile(or("TOKEN_FILE", defaultTokenFile))
	if err != nil {
		return catalog.Report{}, nil, errors.New("read the ServiceAccount token: " + err.Error())
	}
	vault, err := vaultpolicy.Login(ctx, &http.Client{Timeout: timeout}, address, role, strings.TrimSpace(string(token)))
	if err != nil {
		return catalog.Report{}, nil, err
	}
	user, password, err := login(ctx, vault, c.Admin)
	if err != nil {
		return catalog.Report{}, nil, err
	}
	report, err := catalog.Apply(ctx, connector(c, user, password), c)
	if err != nil {
		return report, nil, err
	}
	roles, err := catalog.WriteRoles(ctx, vault, c)
	return report, roles, err
}

var errNoLogin = errors.New("holds no login")

// login reads the admin login from the Secret Store, where the catalog says it is.
func login(ctx context.Context, vault vaultpolicy.Vault, admin catalog.Admin) (string, string, error) {
	held, err := vault.ReadKV(ctx, admin.Path)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", admin.Path, err)
	}
	var user, password string
	if json.Unmarshal(held[admin.User], &user) != nil || json.Unmarshal(held[admin.Password], &password) != nil || user == "" || password == "" {
		return "", "", fmt.Errorf("%s#%s,%s %w", admin.Path, admin.User, admin.Password, errNoLogin)
	}
	return user, password, nil
}

// connector signs in to one database of the datastore as the admin login. The
// password never enters a connection string.
func connector(c catalog.Catalog, user, password string) catalog.Connect {
	return func(ctx context.Context, database string) (*pgx.Conn, error) {
		config, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d dbname=%s sslmode=prefer connect_timeout=%d",
			c.Host, c.Port, database, int(timeout.Seconds())))
		if err != nil {
			return nil, err
		}
		config.User, config.Password = user, password
		return pgx.ConnectConfig(ctx, config)
	}
}
