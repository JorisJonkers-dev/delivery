package main

import (
	"compress/gzip"
	"context"
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

	"filippo.io/age"

	"github.com/JorisJonkers-dev/delivery/internal/offcluster/garagetest"
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

func TestARollbacksCopyOutlivesTheCountAndGoesAfterAWeek(t *testing.T) {
	at := claim(t)
	day := time.Date(2026, 10, 1, 2, 45, 0, 0, time.UTC)
	runs := []struct {
		days     int
		rollback string
	}{{0, ""}, {0, "38526643693"}, {1, ""}, {2, ""}, {8, ""}}
	var after []string
	for i, r := range runs {
		now := func() time.Time { return day.AddDate(0, 0, r.days).Add(time.Duration(i) * time.Minute) }
		values := map[string]string{"BACKUP_RETAIN": "1"}
		if r.rollback != "" {
			values["BACKUP_ROLLBACK"] = r.rollback
		}
		if code := start(context.Background(), slog.New(slog.DiscardHandler), []string{"files"},
			env(values), at, now); code != 0 {
			t.Fatalf("run %d exited %d", i, code)
		}
		after = append(after, strings.Join(held(t, at.backup), " "))
	}
	want := []string{
		"files-20261001T024500Z.tar.gz",
		"files-20261001T024500Z.tar.gz rollback-38526643693-files-20261001T024600Z.tar.gz",
		"files-20261002T024700Z.tar.gz rollback-38526643693-files-20261001T024600Z.tar.gz",
		"files-20261003T024800Z.tar.gz rollback-38526643693-files-20261001T024600Z.tar.gz",
		"files-20261009T024900Z.tar.gz",
	}
	for i := range want {
		if after[i] != want[i] {
			t.Errorf("after run %d holds %s, want %s", i, after[i], want[i])
		}
	}
}

func TestARollbacksRunRefusesARequestDeployKitWouldNot(t *testing.T) {
	at := claim(t)
	if code := start(context.Background(), slog.New(slog.DiscardHandler), []string{"files"},
		env(map[string]string{"BACKUP_RETAIN": "1", "BACKUP_ROLLBACK": "../x"}), at, time.Now); code != 1 {
		t.Fatalf("exited %d", code)
	}
	if names := held(t, at.backup); len(names) != 0 {
		t.Fatalf("wrote %v", names)
	}
}

func TestAnOffClusterPlanItCannotCarryOutKeepsTheLocalGenerationAndFails(t *testing.T) {
	at := claim(t)
	written, _, err := run(context.Background(), []string{"files"},
		env(map[string]string{"BACKUP_RETAIN": "1", "BACKUP_OFF_CLUSTER": "s3://backups/estate"}), at, time.Now())
	if err == nil || !strings.Contains(err.Error(), "kept "+written+" locally") || !strings.Contains(err.Error(), "BACKUP_OFF_CLUSTER_RECIPIENT") {
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

func TestAScheduledRunCopiesOffClusterAndKeepsTheNewestRetainThere(t *testing.T) {
	endpoint := garagetest.Start(t)
	recipient, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	at := claim(t)
	values := map[string]string{
		"BACKUP_RETAIN":                "1",
		"BACKUP_OFF_CLUSTER":           "s3://" + garagetest.Bucket,
		"BACKUP_OFF_CLUSTER_PATH":      "jellyfin/jellyfin/config",
		"BACKUP_OFF_CLUSTER_RECIPIENT": recipient.Recipient().String(),
		"AWS_ENDPOINT_URL":             endpoint,
		"AWS_REGION":                   garagetest.Region,
		"AWS_ACCESS_KEY_ID":            garagetest.AccessKey,
		"AWS_SECRET_ACCESS_KEY":        garagetest.SecretKey,
	}
	day := time.Date(2026, 10, 1, 2, 45, 0, 0, time.UTC)
	if _, _, err := run(t.Context(), []string{"files"}, env(values), at, day); err != nil {
		t.Fatal(err)
	}

	_, removed, err := run(t.Context(), []string{"files"}, env(values), at, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}

	// The first day's copy goes, here and off the cluster, and the staging directory with it.
	if got := strings.Join(removed, " "); got != "files-20261001T024500Z.tar.gz jellyfin/jellyfin/config/files-20261001T024500Z.tar.gz.age" {
		t.Fatalf("removed %s", got)
	}
	if got := strings.Join(held(t, at.backup), " "); got != "files-20261002T024500Z.tar.gz" {
		t.Fatalf("the claim holds %s", got)
	}
	// A Rollback's copy stays on the claim alone, whatever the plan says of off-cluster.
	values["BACKUP_ROLLBACK"] = "abc123"
	values["AWS_ENDPOINT_URL"] = "http://127.0.0.1:1"
	if _, _, err := run(t.Context(), []string{"files"}, env(values), at, day.AddDate(0, 0, 2)); err != nil {
		t.Fatalf("a Rollback's run reached off-cluster: %v", err)
	}
}

func TestWhatIsNotARunIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	cases := map[string]struct {
		args []string
		env  map[string]string
	}{
		"no method":            {nil, map[string]string{"BACKUP_RETAIN": "1"}},
		"two methods":          {[]string{"files", "postgres"}, map[string]string{"BACKUP_RETAIN": "1"}},
		"an unknown method":    {[]string{"mysql"}, map[string]string{"BACKUP_RETAIN": "1"}},
		"no retain":            {[]string{"files"}, nil},
		"postgres, no peer":    {[]string{"postgres"}, map[string]string{"BACKUP_RETAIN": "1", "PGUSER": "backup", "PGPASSWORD": "secret"}},
		"postgres, no account": {[]string{"postgres"}, map[string]string{"BACKUP_RETAIN": "1", "BACKUP_HOST": "postgres", "BACKUP_PORT": "5432"}},
		"rabbitmq, no peer":    {[]string{"rabbitmq"}, map[string]string{"BACKUP_RETAIN": "1", "RABBITMQ_USERNAME": "backup", "RABBITMQ_PASSWORD": "secret"}},
		"rabbitmq, no account": {[]string{"rabbitmq"}, map[string]string{"BACKUP_RETAIN": "1", "BACKUP_HOST": "rabbitmq", "BACKUP_PORT": "15672"}},
		"mongo, no peer":       {[]string{"mongo"}, map[string]string{"BACKUP_RETAIN": "1", "MONGO_USERNAME": "backup", "MONGO_PASSWORD": "secret"}},
		"mongo, no account":    {[]string{"mongo"}, map[string]string{"BACKUP_RETAIN": "1", "BACKUP_HOST": "mongo", "BACKUP_PORT": "27017"}},
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
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho \"-- $PGUSER@$PGHOST:$PGPORT on $PATH\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	written, _, err := run(context.Background(), []string{"postgres"}, env(map[string]string{
		"BACKUP_RETAIN": "3", "BACKUP_HOST": "postgres.data-system.svc.cluster.local", "BACKUP_PORT": "5432",
		"PGUSER": "backup", "PGPASSWORD": "secret", "PG_DUMPALL": command, "PATH": "/usr/local/bin:/usr/bin",
	}), at, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(written, ".sql.gz") {
		t.Fatalf("wrote %s", written)
	}
	if dump := gunzipped(t, written); dump != "-- backup@postgres.data-system.svc.cluster.local:5432 on /usr/local/bin:/usr/bin\n" {
		t.Fatalf("dumped %q", dump)
	}
}

// A Rollback dumps the Project's own database with the Project's own credential
// (deploy-kit spec/v1/30-deliverables.md#a-rollbacks-backup), which reaches that database alone.
func TestPostgresDumpsTheOneDatabaseARunNamesThroughPgDump(t *testing.T) {
	at := claim(t)
	command := filepath.Join(t.TempDir(), "pg_dump")
	//nolint:gosec // G306: an executable stand-in
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho \"-- $* as $PGUSER@$PGHOST:$PGPORT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	written, _, err := run(context.Background(), []string{"postgres"}, env(map[string]string{
		"BACKUP_RETAIN": "1", "BACKUP_ROLLBACK": "39645885317", "BACKUP_DATABASE": "auth_db",
		"BACKUP_HOST": "postgres.data-system.svc.cluster.local", "BACKUP_PORT": "5432",
		"PGUSER": "v-auth-owner", "PGPASSWORD": "secret", "PG_DUMP": command,
	}), at, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if dump := gunzipped(t, written); dump != "-- --no-password --dbname=auth_db as v-auth-owner@postgres.data-system.svc.cluster.local:5432\n" {
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

// The stand-in prints its arguments, its environment and the config file it is handed: the
// password reaches mongodump through that file alone, quoted whatever it holds.
func TestMongoDumpsThroughMongodumpWithThePasswordInItsConfig(t *testing.T) {
	at := claim(t)
	command := filepath.Join(t.TempDir(), "mongodump")
	script := "#!/bin/sh\necho \"$*\"\nenv | sort\nfor a; do case $a in --config=*) cat \"${a#--config=}\";; esac; done\n"
	//nolint:gosec // G306: an executable stand-in
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	written, _, err := run(context.Background(), []string{"mongo"}, env(map[string]string{
		"BACKUP_RETAIN": "3", "BACKUP_HOST": "mongo.data-system.svc.cluster.local", "BACKUP_PORT": "27017",
		"MONGO_USERNAME": "backup", "MONGO_PASSWORD": `hunter"2\x`, "MONGODUMP": command, "PATH": "/usr/bin:/bin",
	}), at, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(written, ".archive.gz") || !strings.HasPrefix(filepath.Base(written), "mongo-") {
		t.Fatalf("wrote %s", written)
	}
	dump := gunzipped(t, written)
	lines := strings.Split(strings.TrimSuffix(dump, "\n"), "\n")
	want := "--host=mongo.data-system.svc.cluster.local --port=27017 --username=backup --authenticationDatabase=admin --archive --config=/dev/fd/3"
	if lines[0] != want {
		t.Fatalf("ran with %q", lines[0])
	}
	if last := lines[len(lines)-1]; last != `password: "hunter\"2\\x"` {
		t.Fatalf("config holds %q", last)
	}
	if before := strings.Join(lines[:len(lines)-1], "\n"); strings.Contains(before, "hunter") || strings.Contains(before, "MONGO_") {
		t.Fatalf("the password reached mongodump other than by its config:\n%s", dump)
	}
}

func TestAFailedMongodumpFailsTheRunWithWhatItSaid(t *testing.T) {
	at := claim(t)
	command := filepath.Join(t.TempDir(), "mongodump")
	//nolint:gosec // G306: an executable stand-in
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho 'authentication failed' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(context.Background(), []string{"mongo"}, env(map[string]string{
		"BACKUP_RETAIN": "1", "BACKUP_HOST": "mongo", "BACKUP_PORT": "27017",
		"MONGO_USERNAME": "backup", "MONGO_PASSWORD": "secret", "MONGODUMP": command,
	}), at, time.Now())
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("error = %v", err)
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
