// Command backup is the entrypoint of every backup method image the Platform document's engines
// name: `backup files`, `backup postgres` or `backup rabbitmq`. Each writes one generation into
// the backup claim at /backup and keeps the newest BACKUP_RETAIN of its method. A Rollback's
// one-shot run, handed BACKUP_ROLLBACK, writes a copy BACKUP_RETAIN never counts, and every run
// deletes such copies once they are 7 days old.
//
//   - files reads the volume, mounted read-only at /data, into a gzipped tar.
//   - postgres runs pg_dumpall against the server at BACKUP_HOST and BACKUP_PORT, as PGUSER with
//     PGPASSWORD, into a gzipped SQL file. Handed BACKUP_DATABASE, it dumps that one database with
//     pg_dump instead: a Rollback's dump of a Project's own database, with its own credential.
//   - rabbitmq reads the broker's definitions from the management API at BACKUP_HOST and
//     BACKUP_PORT, as RABBITMQ_USERNAME with RABBITMQ_PASSWORD.
//
// The render hands a network method its peer as BACKUP_HOST and BACKUP_PORT, and the keys of the
// credential it logs in with as variables (deploy-kit spec/v1/14-platform-intent.md#engines): the
// operator writes PGUSER and PGPASSWORD, or RABBITMQ_USERNAME and RABBITMQ_PASSWORD, at the
// Process's derived path. A method refuses to run without them rather than guess.
//
// Handed BACKUP_OFF_CLUSTER, a scheduled run also copies its generation off the cluster, encrypted
// to BACKUP_OFF_CLUSTER_RECIPIENT, reads it back, and keeps the newest BACKUP_RETAIN there too
// (internal/offcluster). A run that cannot keeps its local generation and fails.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/backup"
	"github.com/JorisJonkers-dev/delivery/internal/offcluster"
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
	generation.Rollback = settings.Rollback
	written, err := backup.Write(at.backup, generation, now, produce)
	if err != nil {
		return "", nil, err
	}
	// A Rollback's copy is never counted against BACKUP_RETAIN, so a one-shot run prunes
	// nothing by count; every run prunes the Rollbacks' copies by age.
	var removed []string
	if settings.Rollback == "" {
		if removed, err = backup.Prune(at.backup, generation, settings.Retain); err != nil {
			return written, nil, err
		}
	}
	expired, err := backup.PruneRollbacks(at.backup, now)
	if err != nil {
		return written, removed, err
	}
	removed = append(removed, expired...)
	// A Rollback's copy is a local one, kept for days; the class's copies are what go off-cluster.
	if settings.OffCluster == "" || settings.Rollback != "" {
		return written, removed, nil
	}
	pruned, err := offCluster(ctx, getenv, at, generation, written, settings.Retain)
	if err != nil {
		return written, removed, fmt.Errorf("kept %s locally: %w", written, err)
	}
	return written, append(removed, pruned...), nil
}

// offCluster copies the generation written at local off the cluster, and prunes the copies there
// to retain, returning the keys it deleted.
func offCluster(ctx context.Context, getenv func(string) string, at paths, generation backup.Generation, local string, retain int) ([]string, error) {
	settings, err := offcluster.ReadSettings(getenv)
	if err != nil {
		return nil, err
	}
	// Each request is signed at the time it is sent: a provider refuses one signed long before.
	store := offcluster.Store{Settings: settings, HTTP: &http.Client{Timeout: timeout}, Now: time.Now}
	if _, err := store.Copy(ctx, local, filepath.Join(at.backup, ".off-cluster-staging")); err != nil {
		return nil, err
	}
	return store.Prune(ctx, func(name string) bool {
		own, sealed := strings.CutSuffix(name, offcluster.Extension)
		return sealed && generation.Matches(own)
	}, retain)
}

// method is what one method writes, and under which name.
func method(ctx context.Context, name string, getenv func(string) string, at paths) (backup.Generation, func(io.Writer) error, error) {
	switch name {
	case "files":
		return backup.Generation{Method: "files", Extension: ".tar.gz"}, func(w io.Writer) error {
			// Beside the generations, on the claim the method writes: the volume is read-only.
			return backup.Archive(ctx, at.data, filepath.Join(at.backup, ".sqlite-staging"), w)
		}, nil
	case "postgres":
		if err := require(getenv, "BACKUP_HOST", "BACKUP_PORT", "PGUSER", "PGPASSWORD"); err != nil {
			return backup.Generation{}, nil, err
		}
		// pg_dumpall reads libpq's own variables, so the peer is handed on under their names. It
		// also finds pg_dump beside itself through PATH, so PATH goes along.
		env := []string{"PGHOST=" + getenv("BACKUP_HOST"), "PGPORT=" + getenv("BACKUP_PORT")}
		for _, key := range []string{"PGUSER", "PGPASSWORD", "PGSSLMODE", "PATH"} {
			if value := getenv(key); value != "" {
				env = append(env, key+"="+value)
			}
		}
		if database := getenv("BACKUP_DATABASE"); database != "" {
			return backup.Generation{Method: "postgres", Extension: ".sql.gz"}, func(w io.Writer) error {
				return backup.Dump(ctx, or(getenv("PG_DUMP"), "pg_dump"), env, w, "--dbname="+database)
			}, nil
		}
		return backup.Generation{Method: "postgres", Extension: ".sql.gz"}, func(w io.Writer) error {
			return backup.Dump(ctx, or(getenv("PG_DUMPALL"), "pg_dumpall"), env, w)
		}, nil
	case "rabbitmq":
		if err := require(getenv, "BACKUP_HOST", "BACKUP_PORT", "RABBITMQ_USERNAME", "RABBITMQ_PASSWORD"); err != nil {
			return backup.Generation{}, nil, err
		}
		// The management surface serves plain HTTP inside the cluster, and the backup identity's
		// policy admits this pod to that one port of that one Process and nowhere else in it.
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
