// Package bucketcatalog applies an object store's bucket catalog the render hands it
// (deploy-kit spec/v1/16-dependencies.md#the-bucket-catalog): each project's key, minted in the
// store or imported into it from the Secret Store, and the buckets that key may read and write.
package bucketcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Catalog is the document the render writes: where the store's admin API answers, the login it
// is applied with, and the keys.
type Catalog struct {
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Admin Admin  `json:"admin"`
	Keys  []Key  `json:"keys"`
}

// Admin is the KV document holding the admin API's token, and its key.
type Admin struct {
	Path  string `json:"path"`
	Token string `json:"token"`
}

// Key is one project's key: its name, the document it is written to, and its buckets.
type Key struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Buckets []string `json:"buckets"`
}

const (
	// A project name is a DNS label, which every derived name starts with.
	project = `[a-z0-9]([a-z0-9-]*[a-z0-9])?`
	segment = `[A-Za-z0-9_][A-Za-z0-9_.-]*`
)

var errNotRendered = errors.New("not what the render writes")

//nolint:gochecknoglobals // compiled constants
var (
	name    = regexp.MustCompile(`^` + project + `$`)
	kvPath  = regexp.MustCompile(`^secret/data(/` + segment + `)+$`)
	keyName = regexp.MustCompile(`^` + segment + `$`)
	// A key's document is `secret/data/<project>/<process>/s3`.
	keyPath = regexp.MustCompile(`^secret/data/(` + project + `)/` + project + `/s3$`)
	// A bucket is `<project>-<name>`, at most 63 characters, as S3 names one.
	bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// Read reads the catalog at path and holds every name to what the render derives, so nothing it
// sends the store or writes to the Secret Store can be anything else.
func Read(path string) (Catalog, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the job's own input, the ConfigMap the render mounts
	if err != nil {
		return Catalog{}, fmt.Errorf("catalog: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var c Catalog
	if err := decoder.Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("catalog: %s does not parse: %w", path, err)
	}
	return c, c.check()
}

func (c Catalog) check() error {
	switch {
	case c.Host == "" || strings.ContainsAny(c.Host, " /?#@:"):
		return fmt.Errorf("catalog: host %q: %w", c.Host, errNotRendered)
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("catalog: port %d: %w", c.Port, errNotRendered)
	case !kvPath.MatchString(c.Admin.Path) || !keyName.MatchString(c.Admin.Token):
		return fmt.Errorf("catalog: admin token %s#%s: %w", c.Admin.Path, c.Admin.Token, errNotRendered)
	case len(c.Keys) == 0:
		return fmt.Errorf("catalog: no key: %w", errNotRendered)
	}
	seen := map[string]bool{}
	for _, k := range c.Keys {
		if err := k.check(); err != nil {
			return err
		}
		if seen[k.Name] {
			return fmt.Errorf("catalog: key %s named twice: %w", k.Name, errNotRendered)
		}
		seen[k.Name] = true
	}
	return nil
}

func (k Key) check() error {
	match := keyPath.FindStringSubmatch(k.Path)
	if !name.MatchString(k.Name) || match == nil || match[1] != k.Name || len(k.Buckets) == 0 {
		return fmt.Errorf("catalog: key %q at %q: %w", k.Name, k.Path, errNotRendered)
	}
	for _, b := range k.Buckets {
		if !bucket.MatchString(b) || !strings.HasPrefix(b, k.Name+"-") {
			return fmt.Errorf("catalog: bucket %q of key %s: %w", b, k.Name, errNotRendered)
		}
	}
	return nil
}
