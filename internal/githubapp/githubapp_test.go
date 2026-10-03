package githubapp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/githubapp"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp/githubtest"
)

func file(t *testing.T, server *githubtest.Server) githubapp.File {
	t.Helper()
	key, err := githubapp.ParseKey(server.PEM())
	if err != nil {
		t.Fatal(err)
	}
	return githubapp.File{
		API:            server.URL + "/",
		Repository:     githubtest.Repository,
		Path:           githubtest.Path,
		Branch:         githubtest.Branch,
		AppID:          githubtest.AppID,
		InstallationID: githubtest.InstallationID,
		Key:            key,
		HTTP:           server.Client(),
		Now:            time.Now,
	}
}

// A document long enough for GitHub to wrap its base64 over several lines.
var long = strings.Repeat("cluster: production # a line of the snapshot\n", 12)

func TestTheAppReadsTheFileAndReplacesWhatItRead(t *testing.T) {
	server := githubtest.New(t, long)
	f := file(t, server)
	ctx := context.Background()

	document, revision, err := f.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(document) != long {
		t.Fatalf("read %q", document)
	}
	if err := f.Commit(ctx, []byte("cluster: next\n"), revision, "chore: capture"); err != nil {
		t.Fatal(err)
	}
	if got := server.Commits(); len(got) != 1 || got[0].Message != "chore: capture" || got[0].Document != "cluster: next\n" {
		t.Fatalf("commits %+v", got)
	}
}

func TestAWriteOverAFileThatMovedIsRefused(t *testing.T) {
	server := githubtest.New(t, long)
	f := file(t, server)
	ctx := context.Background()

	_, revision, err := f.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	server.Set("cluster: written by someone else\n")
	if err := f.Commit(ctx, []byte("cluster: next\n"), revision, "chore: capture"); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("error %v, want the conflict", err)
	}
	if server.Document() != "cluster: written by someone else\n" || len(server.Commits()) != 0 {
		t.Fatal("the other writer's file was overwritten")
	}
}

func TestWhatGitHubRefusesIsReturned(t *testing.T) {
	cases := map[string]struct {
		change func(*githubapp.File)
		want   string
	}{
		"another App's id":      {func(f *githubapp.File) { f.AppID = "1" }, "sign in as the App: 401"},
		"another installation":  {func(f *githubapp.File) { f.InstallationID = "1" }, "sign in as the App: 404"},
		"an expired assertion":  {func(f *githubapp.File) { f.Now = func() time.Time { return time.Now().Add(-time.Hour) } }, "sign in as the App: 401"},
		"a branch not there":    {func(f *githubapp.File) { f.Branch = "gone" }, "read cluster-state.yml: 404"},
		"a file not there":      {func(f *githubapp.File) { f.Path = "other.yml" }, "read other.yml: 404"},
		"an address not GitHub": {func(f *githubapp.File) { f.API = "http://[::1" }, "sign in as the App"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			server := githubtest.New(t, long)
			f := file(t, server)
			c.change(&f)
			if _, _, err := f.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want one naming %q", err, c.want)
			}
			if err := f.Commit(context.Background(), []byte("x"), "0", "m"); err == nil {
				t.Fatal("a write went through")
			}
		})
	}
}

func TestAnswersThatAreNotGitHubsAreRefused(t *testing.T) {
	const granted = `{"token":"t"}` //nolint:gosec // the stand-in server's.
	answers := map[string]map[string]string{
		"no token":           {"token": `{}`, "contents": `{}`},
		"a token not JSON":   {"token": `<html>`, "contents": `{}`},
		"another encoding":   {"token": granted, "contents": `{"content":"x","encoding":"none","sha":"1"}`},
		"content not base64": {"token": granted, "contents": `{"content":"***","encoding":"base64","sha":"1"}`},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(answer["token"]))
					return
				}
				_, _ = w.Write([]byte(answer["contents"]))
			}))
			t.Cleanup(server.Close)
			f := file(t, githubtest.New(t, long))
			f.API, f.HTTP = server.URL, server.Client()
			if _, _, err := f.Snapshot(context.Background()); err == nil {
				t.Fatal("the answer was accepted")
			}
		})
	}
}

func TestParseKey(t *testing.T) {
	server := githubtest.New(t, long)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(server.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := githubapp.ParseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})); err != nil {
		t.Fatalf("a PKCS#8 key was refused: %v", err)
	}

	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecBytes, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string][]byte{
		"not PEM":        []byte("a key"),
		"not a key":      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("nothing")}),
		"not an RSA key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecBytes}),
	}
	for name, pemBytes := range refused {
		if _, err := githubapp.ParseKey(pemBytes); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
