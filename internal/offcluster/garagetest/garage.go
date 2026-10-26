// Package garagetest starts a one-node Garage, the S3-compatible store the estate runs, with one
// bucket and one key, for the tests that write off-cluster copies against a real provider.
package garagetest

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// A released Garage, pinned by tag.
	image = "dxflrs/garage:v2.1.0"
	// Bucket is the one bucket the store holds.
	Bucket = "backups"
	// AccessKey and SecretKey write to it.
	// A key of Garage's own shape, for a store the test starts and discards.
	AccessKey = "GK000000000000000000000001"
	SecretKey = "1111111111111111111111111111111111111111111111111111111111111111"
	// Region is the region the store signs for.
	Region = "garage"
	// AdminToken signs in to the admin API.
	AdminToken = "garagetest-admin" //nolint:gosec // G101: the admin token of a store the test starts and discards
)

// nodeID is a node's id among the log lines its CLI writes beside it.
var nodeID = regexp.MustCompile(`[0-9a-f]{64}`)

const config = `metadata_dir = "/tmp/meta"
data_dir = "/tmp/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "00000000000000000000000000000000000000000000000000000000000000ff"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"

[admin]
api_bind_addr = "[::]:3903"
admin_token = "` + AdminToken + `"
`

// Start starts the store and returns its S3 endpoint. It skips under -short, which runs without
// Docker.
func Start(t *testing.T) string {
	t.Helper()
	s3, _ := StartAdmin(t)
	return s3
}

// StartAdmin starts the store and returns its S3 endpoint and its admin API's.
func StartAdmin(t *testing.T) (s3, admin string) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Docker; skipped with -short")
	}
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"3900/tcp", "3903/tcp"},
			Files: []testcontainers.ContainerFile{{
				Reader: strings.NewReader(config), ContainerFilePath: "/etc/garage.toml", FileMode: 0o644,
			}},
			// The image holds Garage alone, with no shell to check a port from inside.
			WaitingFor: wait.ForListeningPort("3900/tcp").SkipInternalCheck().WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start Garage: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background(), testcontainers.StopTimeout(time.Second)) })
	garage := func(args ...string) string {
		t.Helper()
		var out string
		// The node answers its own CLI a moment after it listens.
		for attempt := range 50 {
			code, reader, err := container.Exec(ctx, append([]string{"/garage"}, args...), tcexec.Multiplexed())
			raw, _ := io.ReadAll(reader)
			out = string(raw)
			if err == nil && code == 0 {
				return out
			}
			if attempt == 49 {
				t.Fatalf("garage %v: %d %v %s", args, code, err, out)
			}
			time.Sleep(200 * time.Millisecond)
		}
		return out
	}
	node := nodeID.FindString(garage("node", "id", "-q"))
	garage("layout", "assign", "-z", "dc1", "-c", "1G", node)
	garage("layout", "apply", "--version", "1")
	garage("key", "import", "--yes", "-n", "test", AccessKey, SecretKey)
	garage("bucket", "create", Bucket)
	garage("bucket", "allow", "--read", "--write", "--owner", Bucket, "--key", "test")
	s3, err = container.PortEndpoint(ctx, "3900/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	admin, err = container.PortEndpoint(ctx, "3903/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	return s3, admin
}
