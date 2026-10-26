package offcluster_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/JorisJonkers-dev/delivery/internal/offcluster"
	"github.com/JorisJonkers-dev/delivery/internal/offcluster/garagetest"
)

// identity is the private half of a recipient a test encrypts to; only a restore holds one.
func identity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// complete is every variable an off-cluster copy reads, pointed at endpoint.
func complete(endpoint, recipient string) map[string]string {
	return map[string]string{
		"BACKUP_OFF_CLUSTER":           "s3://" + garagetest.Bucket + "/estate",
		"BACKUP_OFF_CLUSTER_PATH":      "jellyfin/jellyfin/config",
		"BACKUP_OFF_CLUSTER_RECIPIENT": recipient,
		"AWS_ENDPOINT_URL":             endpoint,
		"AWS_REGION":                   garagetest.Region,
		"AWS_ACCESS_KEY_ID":            garagetest.AccessKey,
		"AWS_SECRET_ACCESS_KEY":        garagetest.SecretKey,
	}
}

func store(t *testing.T, values map[string]string) offcluster.Store {
	t.Helper()
	settings, err := offcluster.ReadSettings(env(values))
	if err != nil {
		t.Fatal(err)
	}
	return offcluster.Store{Settings: settings, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now}
}

// generation writes a local generation named name holding content.
func generation(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fetch reads one object back as the restore would: signed for, then decrypted.
func fetch(t *testing.T, endpoint, key string, id *age.X25519Identity) string {
	t.Helper()
	out, err := offcluster.Fetch(t.Context(), store(t, complete(endpoint, id.Recipient().String())), key)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := age.Decrypt(bytes.NewReader(out), id)
	if err != nil {
		t.Fatalf("decrypt %s: %v", key, err)
	}
	raw, err := io.ReadAll(plain)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestACopyIsWrittenEncryptedReadBackAndRestores(t *testing.T) {
	endpoint := garagetest.Start(t)
	id := identity(t)
	s := store(t, complete(endpoint, id.Recipient().String()))
	local := generation(t, "files-20261026T031500Z.tar.gz", "the volume, as one archive")
	scratch := filepath.Join(t.TempDir(), "staging")

	key, err := s.Copy(t.Context(), local, scratch)
	if err != nil {
		t.Fatal(err)
	}

	if key != "estate/jellyfin/jellyfin/config/files-20261026T031500Z.tar.gz.age" {
		t.Fatalf("key = %s", key)
	}
	if got := fetch(t, endpoint, key, id); got != "the volume, as one archive" {
		t.Fatalf("restored %q", got)
	}
	// What the provider holds is no readable copy.
	sealed, err := offcluster.Fetch(t.Context(), s, key)
	if err != nil || bytes.Contains(sealed, []byte("the volume")) {
		t.Fatalf("the provider holds the plain archive (%v)", err)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the encrypted copy is left on the claim: %v", err)
	}
	// A different private key opens nothing.
	if _, err := age.Decrypt(bytes.NewReader(sealed), identity(t)); err == nil {
		t.Fatal("another key decrypted the copy")
	}
}

func TestPruneKeepsTheNewestCopiesItOwnsAndNothingElse(t *testing.T) {
	endpoint := garagetest.Start(t)
	id := identity(t)
	s := store(t, complete(endpoint, id.Recipient().String()))
	scratch := t.TempDir()
	for _, day := range []string{"24", "26", "25", "23"} {
		name := "files-202610" + day + "T031500Z.tar.gz"
		if _, err := s.Copy(t.Context(), generation(t, name, day), filepath.Join(scratch, day)); err != nil {
			t.Fatal(err)
		}
	}
	// A copy of another method, and one of a deeper path, are not this volume's to prune.
	if _, err := s.Copy(t.Context(), generation(t, "postgres-20261020T031500Z.sql.gz", "x"), filepath.Join(scratch, "p")); err != nil {
		t.Fatal(err)
	}
	deeper := complete(endpoint, id.Recipient().String())
	deeper["BACKUP_OFF_CLUSTER_PATH"] += "/older"
	if _, err := store(t, deeper).Copy(t.Context(), generation(t, "files-20261001T031500Z.tar.gz", "x"), filepath.Join(scratch, "d")); err != nil {
		t.Fatal(err)
	}

	removed, err := s.Prune(t.Context(), func(name string) bool { return strings.HasPrefix(name, "files-") }, 2)
	if err != nil {
		t.Fatal(err)
	}

	prefix := "estate/jellyfin/jellyfin/config/"
	if want := []string{prefix + "files-20261023T031500Z.tar.gz.age", prefix + "files-20261024T031500Z.tar.gz.age"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	if got := fetch(t, endpoint, prefix+"files-20261026T031500Z.tar.gz.age", id); got != "26" {
		t.Fatalf("the newest copy holds %q", got)
	}
	for _, kept := range []string{"postgres-20261020T031500Z.sql.gz.age", "older/files-20261001T031500Z.tar.gz.age", "files-20261025T031500Z.tar.gz.age"} {
		if _, err := offcluster.Fetch(t.Context(), s, prefix+kept); err != nil {
			t.Fatalf("%s is gone: %v", kept, err)
		}
	}
	if _, err := offcluster.Fetch(t.Context(), s, prefix+"files-20261023T031500Z.tar.gz.age"); err == nil {
		t.Fatal("a pruned copy is still there")
	}
	// Under the count, nothing goes.
	if removed, err := s.Prune(t.Context(), func(string) bool { return true }, 10); err != nil || removed != nil {
		t.Fatalf("removed %v, %v", removed, err)
	}
}

func TestAWrongKeyIsRefusedByTheProvider(t *testing.T) {
	endpoint := garagetest.Start(t)
	values := complete(endpoint, identity(t).Recipient().String())
	values["AWS_SECRET_ACCESS_KEY"] = strings.Repeat("f", 64)

	_, err := store(t, values).Copy(t.Context(), generation(t, "files-x.tar.gz", "x"), t.TempDir())

	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

func TestSettingsAreRefusedWhereTheyCannotBeCarriedOut(t *testing.T) {
	recipient := identity(t).Recipient().String()
	cases := map[string]struct {
		change func(map[string]string)
		want   string
	}{
		"no recipient":         {func(v map[string]string) { delete(v, "BACKUP_OFF_CLUSTER_RECIPIENT") }, "[BACKUP_OFF_CLUSTER_RECIPIENT] not set"},
		"no keys":              {func(v map[string]string) { delete(v, "AWS_ACCESS_KEY_ID"); delete(v, "AWS_SECRET_ACCESS_KEY") }, "[AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY] not set"},
		"not s3":               {func(v map[string]string) { v["BACKUP_OFF_CLUSTER"] = "https://bucket/x" }, "is not s3://bucket[/prefix]"},
		"no bucket":            {func(v map[string]string) { v["BACKUP_OFF_CLUSTER"] = "s3:///x" }, "is not s3://bucket[/prefix]"},
		"unparsable":           {func(v map[string]string) { v["BACKUP_OFF_CLUSTER"] = "s3://%zz" }, "is not s3://bucket[/prefix]"},
		"endpoint not http":    {func(v map[string]string) { v["AWS_ENDPOINT_URL"] = "ftp://store" }, "is not an http(s) URL"},
		"endpoint no host":     {func(v map[string]string) { v["AWS_ENDPOINT_URL"] = "https://" }, "is not an http(s) URL"},
		"endpoint unparsable":  {func(v map[string]string) { v["AWS_ENDPOINT_URL"] = "https://%zz" }, "is not an http(s) URL"},
		"recipient not an age": {func(v map[string]string) { v["BACKUP_OFF_CLUSTER_RECIPIENT"] = "ssh-ed25519 AAAA" }, "is not an age public key"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			values := complete("https://store.example", recipient)
			c.change(values)
			if _, err := offcluster.ReadSettings(env(values)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	values := complete("http://store.example:3900", recipient)
	delete(values, "AWS_REGION")
	values["BACKUP_OFF_CLUSTER"] = "s3://bucket"
	settings, err := offcluster.ReadSettings(env(values))
	if err != nil || settings.Region != "us-east-1" || settings.Bucket != "bucket" || settings.Prefix != "jellyfin/jellyfin/config" {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
}

// fake is a provider that answers each request with what answer says, after reading its body.
func fake(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) offcluster.Store {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	return store(t, complete(server.URL, identity(t).Recipient().String()))
}

func TestACopyTheProviderDoesNotHoldWholeFailsTheRun(t *testing.T) {
	s := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("X-Amz-Meta-Sha256", "00")
			w.Header().Set("Content-Length", "3")
		}
	})

	_, err := s.Copy(t.Context(), generation(t, "files-x.tar.gz", "x"), t.TempDir())

	if err == nil || !strings.Contains(err.Error(), `3 bytes with SHA-256 "00"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestEachRequestTheProviderRefusesFailsTheStep(t *testing.T) {
	refuse := func(method string) offcluster.Store {
		return fake(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case method:
				http.Error(w, "<Code>AccessDenied</Code>", http.StatusForbidden)
			case http.MethodGet:
				_, _ = fmt.Fprint(w, `<ListBucketResult><Contents><Key>estate/jellyfin/jellyfin/config/a</Key></Contents></ListBucketResult>`)
			}
		})
	}
	local := generation(t, "files-x.tar.gz", "x")
	if _, err := refuse(http.MethodPut).Copy(t.Context(), local, t.TempDir()); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("put: %v", err)
	}
	if _, err := refuse(http.MethodHead).Copy(t.Context(), local, t.TempDir()); err == nil || !strings.Contains(err.Error(), "read back") {
		t.Fatalf("head: %v", err)
	}
	if _, err := refuse(http.MethodGet).Prune(t.Context(), func(string) bool { return true }, 1); err == nil || !strings.Contains(err.Error(), "list") {
		t.Fatalf("list: %v", err)
	}
	if _, err := refuse(http.MethodDelete).Prune(t.Context(), func(string) bool { return true }, 0); err == nil || !strings.Contains(err.Error(), "delete") {
		t.Fatalf("delete: %v", err)
	}
	garbled := fake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "<unclosed") })
	if _, err := garbled.Prune(t.Context(), func(string) bool { return true }, 1); err == nil {
		t.Fatal("a listing that is no XML was read")
	}
	unreachable := fake(t, func(http.ResponseWriter, *http.Request) {})
	unreachable.Endpoint.Host = "127.0.0.1:1"
	if _, err := unreachable.Copy(t.Context(), local, t.TempDir()); err == nil {
		t.Fatal("an unreachable provider took a copy")
	}
}

func TestAListingIsReadAcrossItsPages(t *testing.T) {
	var tokens []string
	s := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			return
		}
		token := r.URL.Query().Get("continuation-token")
		tokens = append(tokens, token)
		prefix := "estate/jellyfin/jellyfin/config/"
		if token == "" {
			_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>%sfiles-1.age</Key></Contents></ListBucketResult>`, prefix)
			return
		}
		_, _ = fmt.Fprintf(w, `<ListBucketResult><Contents><Key>%sfiles-2.age</Key></Contents></ListBucketResult>`, prefix)
	})

	removed, err := s.Prune(t.Context(), func(string) bool { return true }, 1)

	if err != nil || len(removed) != 1 || !strings.HasSuffix(removed[0], "files-1.age") || !slices.Equal(tokens, []string{"", "next"}) {
		t.Fatalf("removed %v, tokens %v, %v", removed, tokens, err)
	}
}

func TestACopyFailsWhereItsFilesCannotBeRead(t *testing.T) {
	s := fake(t, func(http.ResponseWriter, *http.Request) {})
	if _, err := s.Copy(t.Context(), filepath.Join(t.TempDir(), "absent"), t.TempDir()); err == nil {
		t.Fatal("a missing generation was copied")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Copy(t.Context(), generation(t, "files-x", "x"), filepath.Join(blocked, "staging")); err == nil {
		t.Fatal("a scratch directory under a file was made")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Copy(ctx, generation(t, "files-x", "x"), t.TempDir()); err == nil {
		t.Fatal("a cancelled copy succeeded")
	}
}
