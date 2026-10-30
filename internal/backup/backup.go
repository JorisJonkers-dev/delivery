// Package backup is what each backup method image runs: one generation written into the backup
// claim, the newest BACKUP_RETAIN kept. The contract is deploy-kit's chapter 30 backup row: the
// volume read-only at /data, the backup claim at /backup, BACKUP_RETAIN the count to keep, and
// BACKUP_OFF_CLUSTER the off-cluster destination where the plan copies off-cluster.
//
// A Rollback's one-shot run is also handed BACKUP_ROLLBACK, the request it runs for
// (deploy-kit spec/v1/14-platform-intent.md#engines). Its copy carries a name of its own that
// the BACKUP_RETAIN count never counts, and every run deletes such a copy once it is 7 days old.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RollbackKept is how long a Rollback's copy is kept: deploy-kit's chapter 55 fixes it.
const RollbackKept = 7 * 24 * time.Hour

// rollbackPrefix starts the name of every Rollback's copy, and of nothing else.
const rollbackPrefix = "rollback-"

// request is what a Rollback's request may be: what deploy-kit's pin annotation admits.
var request = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,18}[A-Za-z0-9])?$`)

// stamp is the time a Rollback's copy was written, the last one its name holds.
var stamp = regexp.MustCompile(`-(\d{8}T\d{6}Z)\.`)

// Settings are what the CronJob hands every method, and a one-shot Job the request it runs for.
type Settings struct {
	Retain     int
	OffCluster string
	Rollback   string
}

// ReadSettings reads BACKUP_RETAIN, required and at least one, BACKUP_OFF_CLUSTER, and
// BACKUP_ROLLBACK, which is a request where it is set at all.
func ReadSettings(getenv func(string) string) (Settings, error) {
	raw := getenv("BACKUP_RETAIN")
	retain, err := strconv.Atoi(raw)
	if err != nil || retain < 1 {
		return Settings{}, fmt.Errorf("BACKUP_RETAIN %q is not a count of at least one", raw)
	}
	rollback := getenv("BACKUP_ROLLBACK")
	if rollback != "" && !request.MatchString(rollback) {
		return Settings{}, fmt.Errorf("BACKUP_ROLLBACK %q is not a request", rollback)
	}
	return Settings{Retain: retain, OffCluster: getenv("BACKUP_OFF_CLUSTER"), Rollback: rollback}, nil
}

// Generation names one backup: its method, the extension of what it holds, and for a
// Rollback's copy the request it was taken for.
type Generation struct {
	Method    string
	Extension string
	Rollback  string
}

func (g Generation) name(at time.Time) string {
	own := g.Method + "-" + at.UTC().Format("20060102T150405Z") + g.Extension
	if g.Rollback == "" {
		return own
	}
	return rollbackPrefix + g.Rollback + "-" + own
}

// Matches reports whether name is one of g's generations, never a Rollback's copy.
func (g Generation) Matches(name string) bool {
	return strings.HasPrefix(name, g.Method+"-") && strings.HasSuffix(name, g.Extension)
}

// Write writes one generation into dir through produce, under a temporary name renamed into
// place only once produce succeeded, so a failed run never leaves a generation that looks whole.
func Write(dir string, g Generation, at time.Time, produce func(io.Writer) error) (string, error) {
	final := filepath.Join(dir, g.name(at))
	partial := final + ".part"
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a name this package built inside the claim
	if err != nil {
		return "", fmt.Errorf("open %s: %w", partial, err)
	}
	if err := produce(file); err != nil {
		_ = file.Close()
		_ = os.Remove(partial)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(partial)
		return "", fmt.Errorf("close %s: %w", partial, err)
	}
	if err := os.Rename(partial, final); err != nil {
		return "", fmt.Errorf("rename %s: %w", partial, err)
	}
	return final, nil
}

// Prune removes all but the newest retain generations of g in dir, and returns what it removed.
// A name orders as its time does, so the newest sort last.
func Prune(dir string, g Generation, retain int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var generations []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && g.Matches(entry.Name()) {
			generations = append(generations, entry.Name())
		}
	}
	sort.Strings(generations)
	if len(generations) <= retain {
		return nil, nil
	}
	removed := generations[:len(generations)-retain]
	for _, name := range removed {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return nil, fmt.Errorf("remove %s: %w", name, err)
		}
	}
	return removed, nil
}

// PruneRollbacks removes every Rollback's copy in dir written longer than RollbackKept before
// now, whatever its method and whether or not its run finished it, and returns what it removed. The time is read from the name, which
// the run that wrote it set.
func PruneRollbacks(dir string, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, rollbackPrefix) {
			continue
		}
		found := stamp.FindAllStringSubmatch(name, -1)
		if found == nil {
			continue
		}
		at, err := time.Parse("20060102T150405Z", found[len(found)-1][1])
		if err != nil || now.Sub(at) <= RollbackKept {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return nil, fmt.Errorf("remove %s: %w", name, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// Archive writes every directory, regular file and symbolic link under root as a gzipped tar,
// named relative to root. Anything else, a socket or a device, has no backup worth taking. A
// SQLite database is archived as a consistent copy taken in scratch, a directory of the method's
// own outside root, and the files SQLite keeps beside it are left out (sqlite.go).
func Archive(ctx context.Context, root, scratch string, w io.Writer) error {
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return fmt.Errorf("make the scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	compressed := gzip.NewWriter(w)
	archive := tar.NewWriter(compressed)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil || name == "." {
			return err
		}
		content, skip, err := contentOf(ctx, path, scratch, entry)
		if err != nil || skip {
			return err
		}
		return add(archive, path, content, filepath.ToSlash(name), entry)
	})
	if err != nil {
		return fmt.Errorf("archive %s: %w", root, err)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close the archive: %w", err)
	}
	if err := compressed.Close(); err != nil {
		return fmt.Errorf("close the compression: %w", err)
	}
	return nil
}

// contentOf says where a file's archived content comes from: the file itself, or, for a SQLite
// database, its consistent copy. skip is set for a file SQLite keeps beside a database, which
// the copy has folded in.
func contentOf(ctx context.Context, path, scratch string, entry fs.DirEntry) (content string, skip bool, err error) {
	if !entry.Type().IsRegular() {
		return path, false, nil
	}
	if stem, found := companionOf(path); found {
		database, err := isSQLite(stem)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
		if database {
			return "", true, nil
		}
	}
	database, err := isSQLite(path)
	if err != nil || !database {
		return path, false, err
	}
	copied, err := snapshot(ctx, path, scratch)
	return copied, false, err
}

// add writes one entry: its header from the walked file, its content from content, which is
// the file itself or a consistent copy of it.
func add(archive *tar.Writer, path, content, name string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	link := ""
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	case info.IsDir(), info.Mode().IsRegular():
	default:
		return nil
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = name
	if info.Mode().IsRegular() && content != path {
		copied, err := os.Stat(content)
		if err != nil {
			return err
		}
		header.Size = copied.Size()
	}
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(content) //nolint:gosec // G304: walking the volume is the point
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = io.Copy(archive, file)
	return err
}

// Dump runs pg_dumpall, or pg_dump with the arguments that name its database, which read libpq's
// own variables (PGHOST, PGPORT, PGUSER, PGPASSWORD), and writes the output gzipped.
func Dump(ctx context.Context, command string, env []string, w io.Writer, args ...string) error {
	//nolint:gosec // G204: the command is the method's own pg_dump or pg_dumpall, and args name one database
	dump := exec.CommandContext(ctx, command, append([]string{"--no-password"}, args...)...)
	dump.Env = env
	return compressed(dump, command, w)
}

// MongoDump writes mongodump's archive of every database on the server args name, gzipped. The
// password reaches mongodump as its config file, through a pipe, never as an argument any process
// on the node could read, nor as a file left on the claim.
func MongoDump(ctx context.Context, command string, env []string, password string, w io.Writer, args ...string) error {
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("open the config pipe: %w", err)
	}
	defer func() { _ = reader.Close() }()
	// A JSON string is a YAML double-quoted scalar, so any password survives the config file.
	quoted, err := json.Marshal(password)
	if err != nil {
		_ = writer.Close()
		return fmt.Errorf("quote the password: %w", err)
	}
	// Far below a pipe's buffer, so the write never waits for mongodump to read.
	_, err = fmt.Fprintf(writer, "password: %s\n", quoted)
	if closed := writer.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return fmt.Errorf("write the config pipe: %w", err)
	}
	//nolint:gosec // G204: the command is the method's own mongodump, and args name its peer and login
	dump := exec.CommandContext(ctx, command, append(args, "--archive", "--config=/dev/fd/3")...)
	dump.Env = env
	dump.ExtraFiles = []*os.File{reader}
	return compressed(dump, command, w)
}

// compressed runs dump with its standard output gzipped into w.
func compressed(dump *exec.Cmd, command string, w io.Writer) error {
	compressed := gzip.NewWriter(w)
	var stderr strings.Builder
	dump.Stdout = compressed
	dump.Stderr = &stderr
	if err := dump.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	if err := compressed.Close(); err != nil {
		return fmt.Errorf("close the compression: %w", err)
	}
	return nil
}

// Definitions writes a RabbitMQ broker's definitions, its users, vhosts, queues, exchanges,
// bindings and policies, from the management API at base.
func Definitions(ctx context.Context, client *http.Client, base, username, password string, w io.Writer) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/api/definitions", http.NoBody)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	request.SetBasicAuth(username, password)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("read the definitions: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("read the definitions: %s", response.Status)
	}
	if _, err := io.Copy(w, response.Body); err != nil {
		return fmt.Errorf("read the definitions: %w", err)
	}
	return nil
}
