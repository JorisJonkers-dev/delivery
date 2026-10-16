package catalog_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/catalog"
)

const rendered = `{"admin":{"password":"postgres.password","path":"secret/data/platform/postgres","user":"postgres.user"},` +
	`"connection":"postgres","databases":[{"data":"tribelt-data","name":"tribelt_db","owner":"tribelt-owner"}],` +
	`"host":"postgres.data-system.svc.cluster.local","port":5432}`

func write(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheCatalogTheRenderWritesIsRead(t *testing.T) {
	c, err := catalog.Read(write(t, rendered))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 5432 || c.Admin.User != "postgres.user" || c.Databases[0].Owner != "tribelt-owner" {
		t.Fatalf("catalog = %+v", c)
	}
}

func TestEveryNameIsHeldToWhatTheRenderDerives(t *testing.T) {
	for name, edit := range map[string][2]string{
		"a database not named for a project": {`"name":"tribelt_db"`, `"name":"tribelt"`},
		"an owner of another project":        {`"owner":"tribelt-owner"`, `"owner":"auth-owner"`},
		"a data role with a quote":           {`"data":"tribelt-data"`, `"data":"tribelt-data\""`},
		"a database name with SQL in it":     {`"name":"tribelt_db"`, `"name":"x; DROP DATABASE auth_db; --_db"`},
		"an admin login outside kv":          {`"path":"secret/data/platform/postgres"`, `"path":"sys/policies/acl/root"`},
		"an admin path that climbs out":      {`"path":"secret/data/platform/postgres"`, `"path":"secret/data/../sys/raw"`},
		"a connection with a slash":          {`"connection":"postgres"`, `"connection":"postgres/../x"`},
		"a host with a path":                 {`"host":"postgres.data-system.svc.cluster.local"`, `"host":"evil/x"`},
		"no port":                            {`"port":5432`, `"port":0`},
		"a field the render never writes":    {`"port":5432`, `"port":5432,"sslmode":"disable"`},
	} {
		if _, err := catalog.Read(write(t, strings.Replace(rendered, edit[0], edit[1], 1))); err == nil {
			t.Errorf("%s was read", name)
		}
	}
	if _, err := catalog.Read(write(t, strings.Replace(rendered, `"databases":[`, `"databases":[{"data":"tribelt-data","name":"tribelt_db","owner":"tribelt-owner"},`, 1))); err == nil {
		t.Error("a database named twice was read")
	}
}
