// Command collector captures the ClusterState snapshot once and exits: it is run on a schedule,
// reads three lists from the cluster, and writes one file in the Estate repository when a fact
// changed.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/JorisJonkers-dev/delivery/internal/collector"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const (
	defaultAPI    = "https://api.github.com"
	defaultPath   = "cluster-state.yml"
	defaultBranch = "main"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("collector is not running in a cluster", "error", err)
		return 1
	}
	return start(ctx, logger, os.Getenv, os.ReadFile, config)
}

// start is main minus the cluster it runs in, so tests can hand it one.
func start(ctx context.Context, logger *slog.Logger, getenv func(string) string, readFile func(string) ([]byte, error), config *rest.Config) int {
	c, err := configure(logger, getenv, readFile, config)
	if err != nil {
		logger.Error("collector could not start", "error", err, "version", version)
		return 1
	}
	if _, err := c.Run(ctx); err != nil {
		logger.Error("collector failed", "error", err, "version", version)
		return 1
	}
	return 0
}

func configure(logger *slog.Logger, getenv func(string) string, readFile func(string) ([]byte, error), config *rest.Config) (collector.Collector, error) {
	required := []string{"CLUSTER_NAME", "ESTATE_REPOSITORY", "GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID", "GITHUB_APP_PRIVATE_KEY_FILE"}
	var missing []string
	for _, name := range required {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return collector.Collector{}, fmt.Errorf("not set: %s", strings.Join(missing, ", "))
	}

	pemBytes, err := readFile(getenv("GITHUB_APP_PRIVATE_KEY_FILE"))
	if err != nil {
		return collector.Collector{}, fmt.Errorf("read the App key: %w", err)
	}
	key, err := githubapp.ParseKey(pemBytes)
	if err != nil {
		return collector.Collector{}, err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return collector.Collector{}, fmt.Errorf("reach the cluster: %w", err)
	}

	return collector.Collector{
		Name:    getenv("CLUSTER_NAME"),
		Cluster: collector.Kube{Client: client},
		Repository: githubapp.File{
			API:            orDefault(getenv("GITHUB_API_URL"), defaultAPI),
			Repository:     getenv("ESTATE_REPOSITORY"),
			Path:           orDefault(getenv("SNAPSHOT_PATH"), defaultPath),
			Branch:         orDefault(getenv("ESTATE_BRANCH"), defaultBranch),
			AppID:          getenv("GITHUB_APP_ID"),
			InstallationID: getenv("GITHUB_APP_INSTALLATION_ID"),
			Key:            key,
			HTTP:           &http.Client{Timeout: 30 * time.Second},
			Now:            time.Now,
		},
		Now: time.Now,
		Log: logger,
	}, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
