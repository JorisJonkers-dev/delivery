package bucketcatalog_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/bucketcatalog"
)

// rendered is the catalog deploy-kit's worked example renders for its Garage.
const rendered = `{"admin":{"path":"secret/data/platform/garage","token":"admin-token"},"host":"garage.data-system.svc.cluster.local","keys":[{"buckets":["knowledge-platform-assets"],"name":"knowledge-platform","path":"secret/data/knowledge-platform/garage/s3"}],"port":3903}`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadAcceptsWhatTheRenderWrites(t *testing.T) {
	c, err := bucketcatalog.Read(write(t, rendered))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 3903 || c.Admin.Token != "admin-token" || c.Keys[0].Buckets[0] != "knowledge-platform-assets" {
		t.Fatalf("read %+v", c)
	}
}

func TestReadRefusesWhatTheRenderNeverWrites(t *testing.T) {
	for name, edit := range map[string][2]string{
		"an unknown field":                {`"port":3903`, `"port":3903,"extra":1`},
		"a host with a port":              {`svc.cluster.local"`, `svc.cluster.local:1"`},
		"no host":                         {`"garage.data-system.svc.cluster.local"`, `""`},
		"port zero":                       {`"port":3903`, `"port":0`},
		"a port past the range":           {`"port":3903`, `"port":65536`},
		"an admin path outside the KV":    {`"path":"secret/data/platform/garage"`, `"path":"sys/policies/garage"`},
		"an admin key with a slash":       {`"token":"admin-token"`, `"token":"a/b"`},
		"no key":                          {`"keys":[{"buckets":["knowledge-platform-assets"],"name":"knowledge-platform","path":"secret/data/knowledge-platform/garage/s3"}]`, `"keys":[]`},
		"a key name that is no label":     {`"name":"knowledge-platform"`, `"name":"Knowledge"`},
		"a key written to another's path": {`"path":"secret/data/knowledge-platform/garage/s3"`, `"path":"secret/data/mail/garage/s3"`},
		"a key path that is not s3":       {`"path":"secret/data/knowledge-platform/garage/s3"`, `"path":"secret/data/knowledge-platform/garage/backup"`},
		"a key with no bucket":            {`"buckets":["knowledge-platform-assets"]`, `"buckets":[]`},
		"another project's bucket":        {`"buckets":["knowledge-platform-assets"]`, `"buckets":["mail-assets"]`},
		"a bucket no S3 store names":      {`"buckets":["knowledge-platform-assets"]`, `"buckets":["knowledge-platform-Assets"]`},
	} {
		body := strings.Replace(rendered, edit[0], edit[1], 1)
		if body == rendered {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if _, err := bucketcatalog.Read(write(t, body)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

func TestReadRefusesAKeyNamedTwice(t *testing.T) {
	key := `{"buckets":["knowledge-platform-assets"],"name":"knowledge-platform","path":"secret/data/knowledge-platform/garage/s3"}`
	body := strings.Replace(rendered, `"keys":[`+key, `"keys":[`+key+`,`+key, 1)
	if _, err := bucketcatalog.Read(write(t, body)); err == nil || !strings.Contains(err.Error(), "named twice") {
		t.Fatalf("err %v", err)
	}
}

func TestReadRefusesAFileThatIsNotThere(t *testing.T) {
	if _, err := bucketcatalog.Read(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("read")
	}
	if _, err := bucketcatalog.Read(write(t, "{")); err == nil {
		t.Fatal("read")
	}
}
