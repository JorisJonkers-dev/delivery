// Package backup is what each backup method image runs: one generation written into the backup
// claim, the newest BACKUP_RETAIN kept. The contract is deploy-kit's chapter 30 backup row: the
// volume read-only at /data, the backup claim at /backup, BACKUP_RETAIN the count to keep, and
// BACKUP_OFF_CLUSTER the off-cluster destination where the plan copies off-cluster.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrOffCluster reports a plan that asks for an off-cluster copy, which no method makes yet: the
// local generation is written, and the run still fails, so the missing copy is never silent.
var ErrOffCluster = errors.New("an off-cluster copy is not made yet")

// Settings are what the CronJob hands every method.
type Settings struct {
	Retain     int
	OffCluster string
}

// ReadSettings reads BACKUP_RETAIN, required and at least one, and BACKUP_OFF_CLUSTER.
func ReadSettings(getenv func(string) string) (Settings, error) {
	raw := getenv("BACKUP_RETAIN")
	retain, err := strconv.Atoi(raw)
	if err != nil || retain < 1 {
		return Settings{}, fmt.Errorf("BACKUP_RETAIN %q is not a count of at least one", raw)
	}
	return Settings{Retain: retain, OffCluster: getenv("BACKUP_OFF_CLUSTER")}, nil
}

// Generation names one backup: its method, and the extension of what it holds.
type Generation struct {
	Method    string
	Extension string
}

func (g Generation) name(at time.Time) string {
	return g.Method + "-" + at.UTC().Format("20060102T150405Z") + g.Extension
}

func (g Generation) matches(name string) bool {
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
		if entry.Type().IsRegular() && g.matches(entry.Name()) {
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

// Archive writes every directory, regular file and symbolic link under root as a gzipped tar,
// named relative to root. Anything else, a socket or a device, has no backup worth taking.
func Archive(root string, w io.Writer) error {
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
		return add(archive, path, filepath.ToSlash(name), entry)
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

func add(archive *tar.Writer, path, name string, entry fs.DirEntry) error {
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
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path) //nolint:gosec // G304: walking the volume is the point
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = io.Copy(archive, file)
	return err
}

// Dump runs pg_dumpall, which reads libpq's own variables (PGHOST, PGPORT, PGUSER, PGPASSWORD),
// and writes its output gzipped.
func Dump(ctx context.Context, command string, env []string, w io.Writer) error {
	compressed := gzip.NewWriter(w)
	var stderr strings.Builder
	dump := exec.CommandContext(ctx, command, "--no-password")
	dump.Env = env
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
