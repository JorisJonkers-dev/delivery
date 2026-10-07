package main

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/backup"
)

func claim(t *testing.T) paths {
	t.Helper()
	at := paths{data: t.TempDir(), backup: t.TempDir()}
	if err := os.WriteFile(filepath.Join(at.data, "state.db"), []byte("rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	return at
}

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func held(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestFilesKeepsTheNewestRetainGenerations(t *testing.T) {
	at := claim(t)
	day := time.Date(2026, 10, 1, 2, 45, 0, 0, time.UTC)
	for i := range 3 {
		now := func() time.Time { return day.AddDate(0, 0, i) }
		if code := start(context.Background(), slog.New(slog.DiscardHandler), []string{"files"},
			env(map[string]string{"BACKUP_RETAIN": "2"}), at, now); code != 0 {
			t.Fatalf("run %d exited %d", i, code)
		}
	}
	got := strings.Join(held(t, at.backup), " ")
	if got != "files-20261002T024500Z.tar.gz files-20261003T024500Z.tar.gz" {
		t.Fatalf("holds %s", got)
	}
}

func TestAnOffClusterPlanKeepsTheLocalGenerationAndFails(t *testing.T) {
	at := claim(t)
	written, _, err := run(context.Background(), []string{"files"},
		env(map[string]string{"BACKUP_RETAIN": "1", "BACKUP_OFF_CLUSTER": "s3://backups/estate"}), at, time.Now())
	if !errors.Is(err, backup.ErrOffCluster) || !strings.Contains(err.Error(), "s3://backups/estate") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(written); statErr != nil {
		t.Fatalf("the local generation is gone: %v", statErr)
	}
	if code := start(context.Background(), slog.New(slog.DiscardHandler), []string{"files"},
		env(map[string]string{"BACKUP_RETAIN": "1", "BACKUP_OFF_CLUSTER": "s3://backups/estate"}), at, time.Now); code != 1 {
		t.Fatalf("exited %d", code)
	}
}

func TestWhatIsNotARunIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	cases := map[string]struct {
		args []string
		env  map[string]string
	}{
		"no method":            {nil, map[string]string{"BACKUP_RETAIN": "1"}},
		"two methods":          {[]string{"files", "postgres"}, map[string]string{"BACKUP_RETAIN": "1"}},
		"an unknown method":    {[]string{"mongo"}, map[string]string{"BACKUP_RETAIN": "1"}},
		"no retain":            {[]string{"files"}, nil},
		"postgres, no peer":    {[]string{"postgres"}, map[string]string{"BACKUP_RETAIN": "1", "PGUSER": "backup", "PGPASSWORD": "secret"}},
		"postgres, no account": {[]string{"postgres"}, map[string]string{"BACKUP_RETAIN": "1", "BACKUP_HOST": "postgres", "BACKUP_PORT": "5432"}},
		"rabbitmq, no peer":    {[]string{"rabbitmq"}, map[string]string{"BACKUP_RETAIN": "1", "RABBITMQ_USERNAME": "backup", "RABBITMQ_PASSWORD": "secret"}},
		"rabbitmq, no account": {[]string{"rabbitmq"}, map[string]string{"BACKUP_RETAIN": "1", "BACKUP_HOST": "rabbitmq", "BACKUP_PORT": "15672"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			at := claim(t)
			if _, _, err := run(context.Background(), c.args, env(c.env), at, time.Now()); err == nil {
				t.Fatal("accepted")
			}
			if names := held(t, at.backup); len(names) != 0 {
				t.Fatalf("wrote %v", names)
			}
		})
	}
}

func TestANetworkMethodWithoutItsPeerNamesWhatIsMissing(t *testing.T) {
	_, _, err := run(context.Background(), []string{"postgres"}, env(map[string]string{"BACKUP_RETAIN": "1"}), claim(t), time.Now())
	if err == nil || !strings.Contains(err.Error(), "BACKUP_HOST") || !strings.Contains(err.Error(), "PGPASSWORD") {
		t.Fatalf("error = %v", err)
	}
}

func TestPostgresDumpsThroughPgDumpall(t *testing.T) {
	at := claim(t)
	command := filepath.Join(t.TempDir(), "pg_dumpall")
	//nolint:gosec // G306: an executable stand-in
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho \"-- $PGUSER@$PGHOST:$PGPORT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	written, _, err := run(context.Background(), []string{"postgres"}, env(map[string]string{
		"BACKUP_RETAIN": "3", "BACKUP_HOST": "postgres.data-system.svc.cluster.local", "BACKUP_PORT": "5432",
		"PGUSER": "backup", "PGPASSWORD": "secret", "PG_DUMPALL": command,
	}), at, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(written, ".sql.gz") {
		t.Fatalf("wrote %s", written)
	}
	if dump := gunzipped(t, written); dump != "-- backup@postgres.data-system.svc.cluster.local:5432\n" {
		t.Fatalf("dumped %q", dump)
	}
}

func TestRabbitmqReadsTheDefinitions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"vhosts":[]}`)
	}))
	defer server.Close()
	at := claim(t)
	peer, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	written, _, err := run(context.Background(), []string{"rabbitmq"}, env(map[string]string{
		"BACKUP_RETAIN": "3", "BACKUP_HOST": peer.Hostname(), "BACKUP_PORT": peer.Port(),
		"RABBITMQ_USERNAME": "backup", "RABBITMQ_PASSWORD": "secret",
	}), at, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(written); string(body) != `{"vhosts":[]}` { //nolint:gosec // G304: the generation this run wrote
		t.Fatalf("holds %q", body)
	}
}

func TestAFailedWriteFailsTheRun(t *testing.T) {
	at := paths{data: t.TempDir(), backup: filepath.Join(t.TempDir(), "absent")}
	if code := start(context.Background(), slog.New(slog.DiscardHandler), []string{"files"},
		env(map[string]string{"BACKUP_RETAIN": "1"}), at, time.Now); code != 1 {
		t.Fatalf("exited %d", code)
	}
}

// gunzipped is what the gzipped generation at path holds.
func gunzipped(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path) //nolint:gosec // G304: the generation this run wrote
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
