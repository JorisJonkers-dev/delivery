// Command vault-policy writes the rendered Vault policies and Kubernetes auth roles into Vault,
// once, and exits: it is a Job of the `apps-vso-secrets` Reconcile Unit, so it runs before any
// Application that holds a grant, and a run that fails stops that unit.
//
// Environment: VAULT_ADDR (required), VAULT_ROLE (default policy-admin), POLICIES_DIR (default
// /policies) and TOKEN_FILE (default the pod's own ServiceAccount token).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/vaultpolicy"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const (
	defaultRole      = "policy-admin"
	defaultDirectory = "/policies"
	//nolint:gosec // G101: a path, not a credential
	defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// timeout bounds each call to Vault: a Vault that does not answer fails the run.
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
		logger.Error("vault-policy failed", "error", err, "version", version)
		return 1
	}
	logger.Info("vault-policy applied the render", "written", report.Written, "unchanged", report.Unchanged, "version", version)
	if len(report.Stale) > 0 {
		// Never deleted here: what the render no longer names is a human's to remove.
		logger.Warn("Vault holds what the render no longer names", "stale", report.Stale)
	}
	return 0
}

func run(ctx context.Context, getenv func(string) string, readFile func(string) ([]byte, error)) (vaultpolicy.Report, error) {
	or := func(name, fallback string) string {
		if value := getenv(name); value != "" {
			return value
		}
		return fallback
	}
	address := getenv("VAULT_ADDR")
	if address == "" {
		return vaultpolicy.Report{}, errors.New("VAULT_ADDR is not set")
	}
	// The render is read whole before Vault is touched: half a render is not applied.
	documents, err := vaultpolicy.Read(or("POLICIES_DIR", defaultDirectory))
	if err != nil {
		return vaultpolicy.Report{}, err
	}
	token, err := readFile(or("TOKEN_FILE", defaultTokenFile))
	if err != nil {
		return vaultpolicy.Report{}, errors.New("read the ServiceAccount token: " + err.Error())
	}
	client := &http.Client{Timeout: timeout}
	vault, err := vaultpolicy.Login(ctx, client, address, or("VAULT_ROLE", defaultRole), strings.TrimSpace(string(token)))
	if err != nil {
		return vaultpolicy.Report{}, err
	}
	return vaultpolicy.Apply(ctx, vault, documents)
}
