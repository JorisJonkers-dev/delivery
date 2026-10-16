// Package catalog applies a datastore's database catalog the render hands it
// (deploy-kit spec/v1/16-dependencies.md#applying-the-catalog): each project's
// database and its owner and data roles, an existing database adopted, and the
// Secret Store's database roles that issue their logins.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Catalog is the document the render writes: where the datastore answers, the
// Secret Store connection its roles issue through, the login it is applied
// with, and the databases.
type Catalog struct {
	Host       string     `json:"host"`
	Port       int        `json:"port"`
	Connection string     `json:"connection"`
	Admin      Admin      `json:"admin"`
	Databases  []Database `json:"databases"`
}

// Admin is the KV document holding the administrative login, and its two keys.
type Admin struct {
	Path     string `json:"path"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// Database is one project's database and its two roles.
type Database struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Data  string `json:"data"`
}

const (
	// A project name is a DNS label, which every derived name starts with.
	project = `[a-z0-9]([a-z0-9-]*[a-z0-9])?`
	segment = `[A-Za-z0-9_][A-Za-z0-9_.-]*`
)

var errNotRendered = errors.New("not what the render writes")

//nolint:gochecknoglobals // compiled constants
var (
	database   = regexp.MustCompile(`^(` + project + `)_db$`)
	kvPath     = regexp.MustCompile(`^secret/data(/` + segment + `)+$`)
	keyName    = regexp.MustCompile(`^` + segment + `$`)
	connection = regexp.MustCompile(`^` + project + `$`)
)

// Read reads the catalog at path and holds every name to what the render
// derives, so nothing it puts into SQL or a Vault path can be anything else.
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
	case c.Host == "" || strings.ContainsAny(c.Host, " /?#@"):
		return fmt.Errorf("catalog: host %q: %w", c.Host, errNotRendered)
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("catalog: port %d: %w", c.Port, errNotRendered)
	case !connection.MatchString(c.Connection):
		return fmt.Errorf("catalog: connection %q: %w", c.Connection, errNotRendered)
	case !kvPath.MatchString(c.Admin.Path) || !keyName.MatchString(c.Admin.User) || !keyName.MatchString(c.Admin.Password):
		return fmt.Errorf("catalog: admin login %s#%s,%s: %w", c.Admin.Path, c.Admin.User, c.Admin.Password, errNotRendered)
	case len(c.Databases) == 0:
		return fmt.Errorf("catalog: no database: %w", errNotRendered)
	}
	seen := map[string]bool{}
	for _, d := range c.Databases {
		match := database.FindStringSubmatch(d.Name)
		if match == nil || d.Owner != match[1]+"-owner" || d.Data != match[1]+"-data" {
			return fmt.Errorf("catalog: database %q with roles %q and %q: %w", d.Name, d.Owner, d.Data, errNotRendered)
		}
		if seen[d.Name] {
			return fmt.Errorf("catalog: %s named twice: %w", d.Name, errNotRendered)
		}
		seen[d.Name] = true
	}
	return nil
}
