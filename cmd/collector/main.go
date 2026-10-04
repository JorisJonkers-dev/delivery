// Command collector captures the ClusterState snapshot when it starts and then once every
// interval: it reads three lists from the cluster, and writes one file in the Estate repository
// when a fact changed. It keeps running, and answers whether a capture succeeded lately.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
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
	"github.com/JorisJonkers-dev/delivery/internal/server"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const (
	defaultAPI      = "https://api.github.com"
	defaultPath     = "cluster-state.yml"
	defaultBranch   = "main"
	defaultAddr     = ":8080"
	defaultInterval = 10 * time.Minute
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("collector is not running in a cluster", "error", err)
		return 1
	}
	return start(context.Background(), logger, os.Getenv, os.ReadFile, config)
}

// every is the channel an interval arrives on.
func every(interval time.Duration) <-chan time.Time { return time.NewTicker(interval).C }

// start is main minus the cluster it runs in, so tests can hand it one.
func start(parent context.Context, logger *slog.Logger, getenv func(string) string, readFile func(string) ([]byte, error), config *rest.Config) int {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := configure(logger, getenv, readFile, config)
	if err != nil {
		logger.Error("collector could not start", "error", err, "version", version)
		return 1
	}
	interval, err := intervalOf(getenv("INTERVAL"))
	if err != nil {
		logger.Error("collector could not start", "error", err, "version", version)
		return 1
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", orDefault(getenv("ADDR"), defaultAddr))
	if err != nil {
		logger.Error("collector could not listen", "error", err)
		return 1
	}

	loop := collector.NewLoop(c.Run, interval, time.Now, every, logger)
	captured := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(captured)
	}()
	served := server.New(logger, version, loop.Routes).Serve(ctx, ln)
	// The server stops when it is told to or when it cannot serve; either way the captures stop with it.
	stop()
	<-captured
	if served != nil {
		logger.Error("collector stopped", "error", served)
		return 1
	}
	return 0
}

// intervalOf reads how often to capture: a Go duration, ten minutes when it is not set.
func intervalOf(value string) (time.Duration, error) {
	if value == "" {
		return defaultInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval <= 0 {
		return 0, fmt.Errorf("INTERVAL %q is not a duration above zero", value)
	}
	return interval, nil
}

func configure(logger *slog.Logger, getenv func(string) string, readFile func(string) ([]byte, error), config *rest.Config) (collector.Collector, error) {
	required := []string{"CLUSTER_NAME", "ESTATE_REPOSITORY", "GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID"}
	var missing []string
	for _, name := range required {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	// The App's key arrives the way its grant is delivered: as a variable, or as a file.
	if getenv("GITHUB_APP_PRIVATE_KEY") == "" && getenv("GITHUB_APP_PRIVATE_KEY_FILE") == "" {
		missing = append(missing, "GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE")
	}
	if len(missing) > 0 {
		return collector.Collector{}, fmt.Errorf("not set: %s", strings.Join(missing, ", "))
	}

	pemBytes := []byte(getenv("GITHUB_APP_PRIVATE_KEY"))
	if len(pemBytes) == 0 {
		read, err := readFile(getenv("GITHUB_APP_PRIVATE_KEY_FILE"))
		if err != nil {
			return collector.Collector{}, fmt.Errorf("read the App key: %w", err)
		}
		pemBytes = read
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
