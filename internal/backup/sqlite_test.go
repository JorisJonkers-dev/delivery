package backup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/JorisJonkers-dev/delivery/internal/backup"
)

// entries is every regular file an archive holds, by name.
func entries(t *testing.T, archived []byte) map[string][]byte {
	t.Helper()
	compressed, err := gzip.NewReader(bytes.NewReader(archived))
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(compressed)
	held := map[string][]byte{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return held
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(body)) != header.Size {
			t.Fatalf("%s holds %d bytes, its header %d", header.Name, len(body), header.Size)
		}
		if header.Typeflag == tar.TypeReg {
			held[header.Name] = body
		}
	}
}

// live is a database an app is writing: write-ahead mode, nothing checkpointed, and the
// connection still open, so its newest rows are in the log alone.
func live(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE watched (title TEXT)",
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	for i := range rows {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO watched VALUES (?)", strings.Repeat("x", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestALiveSQLiteDatabaseIsArchivedAsOneConsistentFile(t *testing.T) {
	root, scratch := t.TempDir(), filepath.Join(t.TempDir(), "staging")
	database := filepath.Join(root, "data", "library.db")
	if err := os.MkdirAll(filepath.Dir(database), 0o700); err != nil {
		t.Fatal(err)
	}
	live(t, database, 25)
	if _, err := os.Stat(database + "-wal"); err != nil {
		t.Fatalf("the live database has no log to fold in: %v", err)
	}
	// A file that only ends like a companion, beside no database, is archived as it is.
	if err := os.WriteFile(filepath.Join(root, "data", "notes-wal"), []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	if err := backup.Archive(t.Context(), root, scratch, &out); err != nil {
		t.Fatal(err)
	}

	held := entries(t, out.Bytes())
	for name := range held {
		if name == "data/library.db-wal" || name == "data/library.db-shm" {
			t.Fatalf("the archive holds %s, which the copy folds in", name)
		}
	}
	if string(held["data/notes-wal"]) != "plain" {
		t.Fatalf("notes-wal = %q", held["data/notes-wal"])
	}
	restored := filepath.Join(t.TempDir(), "library.db")
	if err := os.WriteFile(restored, held["data/library.db"], 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+restored+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM watched").Scan(&count); err != nil || count != 25 {
		t.Fatalf("the restored copy holds %d rows, %v", count, err)
	}
	// The method leaves nothing behind on the claim, and never touches the live database.
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the scratch directory is left: %v", err)
	}
	if _, err := os.Stat(database + "-wal"); err != nil {
		t.Fatalf("the live database's log was touched: %v", err)
	}
}

func TestADatabaseThatNeverCopiesCleanlyFailsTheRun(t *testing.T) {
	defer backup.SettleQuickly()()
	root := t.TempDir()
	// A SQLite header over pages that are no database: every copy fails the check.
	torn := append([]byte("SQLite format 3\x00"), bytes.Repeat([]byte{0xff}, 4096)...)
	if err := os.WriteFile(filepath.Join(root, "app.db"), torn, 0o600); err != nil {
		t.Fatal(err)
	}

	err := backup.Archive(t.Context(), root, filepath.Join(t.TempDir(), "staging"), io.Discard)

	if err == nil || !strings.Contains(err.Error(), "consistently, 3 times") {
		t.Fatalf("err = %v", err)
	}
}

func TestAFileShorterThanTheHeaderIsNoDatabase(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tiny"), []byte("SQL"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := backup.Archive(t.Context(), root, filepath.Join(t.TempDir(), "staging"), &out); err != nil {
		t.Fatal(err)
	}
	if string(entries(t, out.Bytes())["tiny"]) != "SQL" {
		t.Fatal("a short file was not archived as it is")
	}
}
