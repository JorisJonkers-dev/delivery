// Command release-gate answers Flagger's webhooks: whether a member's new version may start,
// whether it passes an analysis iteration, and whether it may be promoted. It reads the cluster
// it runs in and nothing else, serves liveness and readiness probes, and drains on SIGTERM.
//
// Environment: ADDR (default :8080).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
	"github.com/JorisJonkers-dev/delivery/internal/server"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const defaultAddr = ":8080"

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("release-gate is not running in a cluster", "error", err)
		return 1
	}
	return start(context.Background(), logger, os.Getenv, config)
}

// start is main minus the cluster it runs in, so tests can hand it one.
func start(parent context.Context, logger *slog.Logger, getenv func(string) string, config *rest.Config) int {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cluster, err := reader(config)
	if err != nil {
		logger.Error("release-gate could not start", "error", err, "version", version)
		return 1
	}
	addr := getenv("ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		logger.Error("release-gate could not listen", "error", err)
		return 1
	}
	routes := func(mux *http.ServeMux) { gate.New(cluster).Routes(mux, logger) }
	if err := server.New(logger, version, routes).Serve(ctx, ln); err != nil {
		logger.Error("release-gate stopped", "error", err)
		return 1
	}
	return 0
}

// reader builds the two clients the gate reads the cluster through.
func reader(config *rest.Config) (gate.Kube, error) {
	client, typed := kubernetes.NewForConfig(config)
	dyn, untyped := dynamic.NewForConfig(config)
	if err := errors.Join(typed, untyped); err != nil {
		return gate.Kube{}, fmt.Errorf("reach the cluster: %w", err)
	}
	return gate.Kube{Client: client, Dynamic: dyn}, nil
}
