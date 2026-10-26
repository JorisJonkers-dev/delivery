package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the database/sql driver "sqlite", pure Go, so the image stays static
)

// A SQLite database on the volume is copied consistently (deploy-kit#470, option B): the
// volume is mounted read-only, which rules out SQLite's online backup API, so the database and
// its write-ahead log are copied together, the log is folded into the copy, and the copy is held
// to SQLite's own integrity check. A copy a write tore is taken again; one that never passes
// fails the run. The copy can miss the last moments of writes, and is never a torn file.

// sqliteHeader starts every SQLite database file.
var sqliteHeader = []byte("SQLite format 3\x00") //nolint:gochecknoglobals // a constant in all but type

// sqliteCompanions are the files SQLite keeps beside a database while it is in use. The
// snapshot folds them in, so the archive holds none of them.
var sqliteCompanions = []string{"-wal", "-shm", "-journal"} //nolint:gochecknoglobals // a constant list

// snapshotAttempts is how often a copy is taken before the run fails.
const snapshotAttempts = 3

// snapshotWait is how long the method waits between two copies, for the writer to settle.
var snapshotWait = time.Second //nolint:gochecknoglobals // a constant, shortened in tests

var errTorn = errors.New("the copy fails SQLite's integrity check")

// isSQLite reports whether the regular file at path is a SQLite database, by its header.
func isSQLite(path string) (bool, error) {
	file, err := os.Open(path) //nolint:gosec // G304: walking the volume is the point
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(file, header); err != nil {
		// A file shorter than the header is no database.
		return false, nil //nolint:nilerr // short is an answer, not a failure
	}
	return bytes.Equal(header, sqliteHeader), nil
}

// companionOf reports whether path is a file SQLite keeps beside a database at its stem.
func companionOf(path string) (string, bool) {
	for _, suffix := range sqliteCompanions {
		if stem, found := strings.CutSuffix(path, suffix); found {
			return stem, true
		}
	}
	return "", false
}

// snapshot copies the database at path, with its write-ahead log, into scratch, folds the log
// in, and returns the one-file copy once it passes SQLite's integrity check.
func snapshot(ctx context.Context, path, scratch string) (string, error) {
	var last error
	for attempt := 1; attempt <= snapshotAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(snapshotWait)
		}
		copied, err := snapshotOnce(ctx, path, scratch)
		if err == nil {
			return copied, nil
		}
		last = err
	}
	return "", fmt.Errorf("copy %s consistently, %d times: %w", path, snapshotAttempts, last)
}

func snapshotOnce(ctx context.Context, path, scratch string) (string, error) {
	dir, err := os.MkdirTemp(scratch, "sqlite-")
	if err != nil {
		return "", err
	}
	copied := filepath.Join(dir, filepath.Base(path))
	if err := copyFile(path, copied); err != nil {
		return "", err
	}
	// The log holds what the database file does not yet; without it a copy taken mid-write
	// would lose committed transactions.
	if err := copyFile(path+"-wal", copied+"-wal"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	db, err := sql.Open("sqlite", "file:"+copied+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return "", fmt.Errorf("fold the log into the copy: %w", err)
	}
	var verdict string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&verdict); err != nil {
		return "", fmt.Errorf("check the copy: %w", err)
	}
	if verdict != "ok" {
		return "", fmt.Errorf("%w: %s", errTorn, verdict)
	}
	// One file: the archive holds the database alone, and restores without its log.
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		return "", fmt.Errorf("leave write-ahead mode on the copy: %w", err)
	}
	return copied, db.Close()
}

func copyFile(from, to string) error {
	source, err := os.Open(from) //nolint:gosec // G304: walking the volume is the point
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the method's own scratch directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return err
	}
	return target.Close()
}
