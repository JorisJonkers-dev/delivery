// Command backup is the entrypoint of every backup method image the Platform document's engines
// name: `backup files`, `backup postgres` or `backup rabbitmq`. Each writes one generation into
// the backup claim at /backup and keeps the newest BACKUP_RETAIN of its method.
//
//   - files reads the volume, mounted read-only at /data, into a gzipped tar.
//   - postgres runs pg_dumpall against the server at BACKUP_HOST and BACKUP_PORT, as PGUSER with
//     PGPASSWORD, into a gzipped SQL file.
//   - rabbitmq reads the broker's definitions from the management API at BACKUP_HOST and
//     BACKUP_PORT, as RABBITMQ_USERNAME with RABBITMQ_PASSWORD.
//
// The render hands a network method its peer as BACKUP_HOST and BACKUP_PORT, and the keys of the
// credential it logs in with as variables (deploy-kit spec/v1/14-platform-intent.md#engines): the
// operator writes PGUSER and PGPASSWORD, or RABBITMQ_USERNAME and RABBITMQ_PASSWORD, at the
// Process's derived path. A method refuses to run without them rather than guess.
// BACKUP_OFF_CLUSTER is not copied to yet: the local generation is written and the run fails.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/backup"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

// timeout bounds a whole run: a peer that never answers fails the backup rather than hold it.
const timeout = 2 * time.Hour

// paths are the claim the CronJob mounts, fixed by the contract; tests point them elsewhere.
type paths struct {
	data   string
	backup string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	os.Exit(start(context.Background(), logger, os.Args[1:], os.Getenv, paths{data: "/data", backup: "/backup"}, time.Now))
}

// start is main minus the process it runs in, so tests can drive it.
func start(parent context.Context, logger *slog.Logger, args []string, getenv func(string) string, at paths, now func() time.Time) int {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	written, removed, err := run(ctx, args, getenv, at, now())
	if written != "" {
		logger.Info("backup written", "generation", written, "removed", removed, "version", version)
	}
	if err != nil {
		logger.Error("backup failed", "error", err, "version", version)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, getenv func(string) string, at paths, now time.Time) (string, []string, error) {
	if len(args) != 1 {
		return "", nil, errors.New("usage: backup files|postgres|rabbitmq")
	}
	settings, err := backup.ReadSettings(getenv)
	if err != nil {
		return "", nil, err
	}
	generation, produce, err := method(ctx, args[0], getenv, at)
	if err != nil {
		return "", nil, err
	}
	written, err := backup.Write(at.backup, generation, now, produce)
	if err != nil {
		return "", nil, err
	}
	removed, err := backup.Prune(at.backup, generation, settings.Retain)
	if err != nil {
		return written, nil, err
	}
	if settings.OffCluster != "" {
		return written, removed, fmt.Errorf("%w: kept %s locally, not copied to %s", backup.ErrOffCluster, written, settings.OffCluster)
	}
	return written, removed, nil
}

// method is what one method writes, and under which name.
func method(ctx context.Context, name string, getenv func(string) string, at paths) (backup.Generation, func(io.Writer) error, error) {
	switch name {
	case "files":
		return backup.Generation{Method: "files", Extension: ".tar.gz"}, func(w io.Writer) error {
			return backup.Archive(at.data, w)
		}, nil
	case "postgres":
		if err := require(getenv, "BACKUP_HOST", "BACKUP_PORT", "PGUSER", "PGPASSWORD"); err != nil {
			return backup.Generation{}, nil, err
		}
		// pg_dumpall reads libpq's own variables, so the peer is handed on under their names.
		env := []string{"PGHOST=" + getenv("BACKUP_HOST"), "PGPORT=" + getenv("BACKUP_PORT")}
		for _, key := range []string{"PGUSER", "PGPASSWORD", "PGSSLMODE"} {
			if value := getenv(key); value != "" {
				env = append(env, key+"="+value)
			}
		}
		return backup.Generation{Method: "postgres", Extension: ".sql.gz"}, func(w io.Writer) error {
			return backup.Dump(ctx, or(getenv("PG_DUMPALL"), "pg_dumpall"), env, w)
		}, nil
	case "rabbitmq":
		if err := require(getenv, "BACKUP_HOST", "BACKUP_PORT", "RABBITMQ_USERNAME", "RABBITMQ_PASSWORD"); err != nil {
			return backup.Generation{}, nil, err
		}
		management := "http://" + net.JoinHostPort(getenv("BACKUP_HOST"), getenv("BACKUP_PORT"))
		return backup.Generation{Method: "rabbitmq", Extension: ".json"}, func(w io.Writer) error {
			return backup.Definitions(ctx, http.DefaultClient, management,
				getenv("RABBITMQ_USERNAME"), getenv("RABBITMQ_PASSWORD"), w)
		}, nil
	default:
		return backup.Generation{}, nil, fmt.Errorf("no backup method %q: files, postgres or rabbitmq", name)
	}
}

func require(getenv func(string) string, names ...string) error {
	var missing []string
	for _, name := range names {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%v not set: the render hands a network method its peer and its credential's keys (deploy-kit spec/v1/14-platform-intent.md#engines)", missing)
	}
	return nil
}

func or(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
