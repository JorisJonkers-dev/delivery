package backup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/backup"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestReadSettings(t *testing.T) {
	got, err := backup.ReadSettings(env(map[string]string{"BACKUP_RETAIN": "7", "BACKUP_OFF_CLUSTER": "s3://b/p"}))
	if err != nil || got != (backup.Settings{Retain: 7, OffCluster: "s3://b/p"}) {
		t.Fatalf("ReadSettings = %+v, %v", got, err)
	}
	for _, raw := range []string{"", "0", "-1", "seven"} {
		if _, err := backup.ReadSettings(env(map[string]string{"BACKUP_RETAIN": raw})); err == nil {
			t.Errorf("BACKUP_RETAIN %q was accepted", raw)
		}
	}
}

var files = backup.Generation{Method: "files", Extension: ".tar.gz"}

func TestWriteNamesTheGenerationByItsTime(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 7, 2, 45, 0, 0, time.FixedZone("CEST", 2*3600))
	written, err := backup.Write(dir, files, at, func(w io.Writer) error {
		_, err := io.WriteString(w, "held")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "files-20261007T004500Z.tar.gz"); written != want {
		t.Fatalf("wrote %s, want %s", written, want)
	}
	if body, _ := os.ReadFile(written); string(body) != "held" { //nolint:gosec // G304: the generation this test wrote
		t.Fatalf("holds %q", body)
	}
}

func TestAFailedWriteLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	failed := errors.New("peer gone")
	_, err := backup.Write(dir, files, time.Now(), func(w io.Writer) error {
		_, _ = io.WriteString(w, "half")
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("error = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
}

func TestWriteRefusesWhatItCannotOpen(t *testing.T) {
	if _, err := backup.Write(filepath.Join(t.TempDir(), "absent"), files, time.Now(), func(io.Writer) error { return nil }); err == nil {
		t.Fatal("wrote into a directory that does not exist")
	}
}

func TestPruneKeepsTheNewestOfItsOwnMethod(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"files-20261001T000000Z.tar.gz", "files-20261003T000000Z.tar.gz", "files-20261002T000000Z.tar.gz",
		"postgres-20261001T000000Z.sql.gz", "files-20261004T000000Z.tar.gz.part", "notes.txt",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "files-20261005T000000Z.tar.gz"), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := backup.Prune(dir, files, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"files-20261001T000000Z.tar.gz"}) {
		t.Fatalf("removed %v", removed)
	}
	if removed, _ := backup.Prune(dir, files, 2); removed != nil {
		t.Fatalf("a second prune removed %v", removed)
	}
	if _, err := backup.Prune(filepath.Join(dir, "absent"), files, 1); err == nil {
		t.Fatal("pruned a directory that does not exist")
	}
}

func TestArchiveHoldsTheTreeRelativeToItsRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "library", "2026"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "library", "2026", "photo.jpg"), []byte("pixels"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("library/2026/photo.jpg", filepath.Join(root, "latest")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := backup.Archive(root, &out); err != nil {
		t.Fatal(err)
	}
	compressed, err := gzip.NewReader(&out)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(compressed)
	held := map[string]string{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(archive)
		held[header.Name] = string(header.Typeflag) + ":" + header.Linkname + ":" + string(body)
	}
	want := map[string]string{
		"latest":                 "2:library/2026/photo.jpg:",
		"library":                "5::",
		"library/2026":           "5::",
		"library/2026/photo.jpg": "0::pixels",
	}
	if len(held) != len(want) {
		t.Fatalf("held %v", held)
	}
	for name, entry := range want {
		if held[name] != entry {
			t.Errorf("%s = %q, want %q", name, held[name], entry)
		}
	}
	if err := backup.Archive(filepath.Join(root, "absent"), io.Discard); err == nil {
		t.Fatal("archived a root that does not exist")
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pg_dumpall")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // G306: an executable stand-in
		t.Fatal(err)
	}
	return path
}

func TestDumpGzipsWhatPgDumpallWrites(t *testing.T) {
	command := script(t, `[ "$1" = --no-password ] && echo "-- dump of $PGHOST"`)
	var out bytes.Buffer
	if err := backup.Dump(context.Background(), command, []string{"PGHOST=postgres"}, &out); err != nil {
		t.Fatal(err)
	}
	compressed, err := gzip.NewReader(&out)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(compressed)
	if string(body) != "-- dump of postgres\n" {
		t.Fatalf("dumped %q", body)
	}
}

func TestAFailedDumpSaysWhy(t *testing.T) {
	command := script(t, `echo 'server version mismatch' >&2; exit 1`)
	err := backup.Dump(context.Background(), command, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "server version mismatch") {
		t.Fatalf("error = %v", err)
	}
}

func TestDefinitionsAreReadAsTheBackupUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		if r.URL.Path != "/api/definitions" || user != "backup" || password != "secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"queues":[]}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	if err := backup.Definitions(context.Background(), server.Client(), server.URL+"/", "backup", "secret", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"queues":[]}` {
		t.Fatalf("read %q", out.String())
	}
	if err := backup.Definitions(context.Background(), server.Client(), server.URL, "backup", "wrong", io.Discard); err == nil {
		t.Fatal("a refused login was read as definitions")
	}
	if err := backup.Definitions(context.Background(), server.Client(), "http://127.0.0.1:1", "backup", "secret", io.Discard); err == nil {
		t.Fatal("a broker that does not answer was read as definitions")
	}
	if err := backup.Definitions(context.Background(), server.Client(), "://", "backup", "secret", io.Discard); err == nil {
		t.Fatal("a malformed address was read")
	}
}
