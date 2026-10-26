package bucketcatalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/bucketcatalog"
	"github.com/JorisJonkers-dev/delivery/internal/offcluster/garagetest"
)

// store is a Secret Store held in memory, which writes a document only where none is, as
// check-and-set at version 0 does.
type store map[string]map[string]json.RawMessage

var errExists = errors.New("check-and-set: the document exists")

func (s store) ReadKV(_ context.Context, path string) (map[string]json.RawMessage, error) {
	return s[path], nil
}

func (s store) CreateKV(_ context.Context, path string, data map[string]string) error {
	if _, ok := s[path]; ok {
		return errExists
	}
	held := map[string]json.RawMessage{}
	for key, value := range data {
		raw, _ := json.Marshal(value)
		held[key] = raw
	}
	s[path] = held
	return nil
}

func doc(id, secret string) map[string]json.RawMessage {
	raw := func(value string) json.RawMessage { encoded, _ := json.Marshal(value); return encoded }
	return map[string]json.RawMessage{"access-key-id": raw(id), "secret-access-key": raw(secret)}
}

func garage(t *testing.T) bucketcatalog.Garage {
	t.Helper()
	_, admin := garagetest.StartAdmin(t)
	return bucketcatalog.Garage{Address: admin, Token: garagetest.AdminToken, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func keyOf(project string, buckets ...string) bucketcatalog.Key {
	return bucketcatalog.Key{Name: project, Path: "secret/data/" + project + "/garage/s3", Buckets: buckets}
}

// grants returns the key ids the store grants on a bucket, each with read, write and owner.
func grants(t *testing.T, g bucketcatalog.Garage, alias string) map[string][3]bool {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, g.Address+"/v2/GetBucketInfo?"+url.Values{"globalAlias": {alias}}.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+g.Token)
	res, err := g.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var bucket struct {
		Keys []struct {
			ID          string `json:"accessKeyId"`
			Permissions struct {
				Read, Write, Owner bool
			} `json:"permissions"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&bucket); err != nil {
		t.Fatal(err)
	}
	out := map[string][3]bool{}
	for _, k := range bucket.Keys {
		out[k.ID] = [3]bool{k.Permissions.Read, k.Permissions.Write, k.Permissions.Owner}
	}
	return out
}

func TestApplyMintsAKeyAndItsBucketsOnceAndImportsAKeyTheSecretStoreHolds(t *testing.T) {
	g := garage(t)
	s := store{
		// A key the old path pre-generated, which the store does not hold yet.
		"secret/data/mail/garage/s3": doc("GK000000000000000000000003", "3333333333333333333333333333333333333333333333333333333333333333"),
	}
	c := bucketcatalog.Catalog{Keys: []bucketcatalog.Key{
		keyOf("knowledge", "knowledge-archive", "knowledge-assets"),
		keyOf("mail", "mail-spool"),
	}}

	report, err := bucketcatalog.Apply(t.Context(), g, s, c)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.Minted, []string{"knowledge"}) || !slices.Equal(report.Imported, []string{"mail"}) ||
		!slices.Equal(report.Buckets, []string{"knowledge-archive", "knowledge-assets", "mail-spool"}) {
		t.Fatalf("first run %+v", report)
	}
	var minted string
	if json.Unmarshal(s["secret/data/knowledge/garage/s3"]["access-key-id"], &minted) != nil || minted == "" {
		t.Fatalf("no minted key written: %v", s["secret/data/knowledge/garage/s3"])
	}
	for alias, id := range map[string]string{
		"knowledge-archive": minted, "knowledge-assets": minted, "mail-spool": "GK000000000000000000000003",
	} {
		if got := grants(t, g, alias); len(got) != 1 || got[id] != [3]bool{true, true, false} {
			t.Errorf("%s grants %v, want read and write for %s", alias, got, id)
		}
	}

	again, err := bucketcatalog.Apply(t.Context(), g, s, c)
	if err != nil || len(again.Minted)+len(again.Imported)+len(again.Buckets) != 0 {
		t.Fatalf("second run %+v, %v", again, err)
	}
}

func TestApplyWritesDownAKeyARunBeforeMintedAndCouldNotWrite(t *testing.T) {
	g := garage(t)
	held, err := g.CreateKey(t.Context(), "utility")
	if err != nil {
		t.Fatal(err)
	}
	s := store{}
	report, err := bucketcatalog.Apply(t.Context(), g, s, bucketcatalog.Catalog{Keys: []bucketcatalog.Key{keyOf("utility", "utility-backups")}})
	if err != nil || !slices.Equal(report.Minted, []string{"utility"}) {
		t.Fatalf("%+v, %v", report, err)
	}
	want := doc(held.ID, held.Secret)
	for key, value := range want {
		if string(s["secret/data/utility/garage/s3"][key]) != string(value) {
			t.Fatalf("wrote %v, want the key the store holds", s["secret/data/utility/garage/s3"])
		}
	}
	if ids, _ := g.KeyIDs(t.Context(), "utility"); len(ids) != 1 {
		t.Fatalf("the store holds %d keys named utility", len(ids))
	}
}

func TestApplyStopsWhereItCannotTellWhichKeyIsTheProjects(t *testing.T) {
	g := garage(t)
	if err := g.ImportKey(t.Context(), "edge", bucketcatalog.Credentials{ID: "GK000000000000000000000004", Secret: "4444444444444444444444444444444444444444444444444444444444444444"}); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		store store
		setup func()
	}{
		"half a key in the Secret Store": {
			store: store{"secret/data/edge/garage/s3": {"access-key-id": json.RawMessage(`"GK000000000000000000000004"`)}},
		},
		"a secret the store holds differently": {
			store: store{"secret/data/edge/garage/s3": doc("GK000000000000000000000004", "5555555555555555555555555555555555555555555555555555555555555555")},
		},
		"two keys under the project's name": {
			store: store{},
			setup: func() {
				if _, err := g.CreateKey(t.Context(), "edge"); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			_, err := bucketcatalog.Apply(t.Context(), g, tc.store, bucketcatalog.Catalog{Keys: []bucketcatalog.Key{keyOf("edge", "edge-assets")}})
			if err == nil {
				t.Fatal("applied")
			}
		})
	}
}

func TestApplyFailsWithAWrongAdminToken(t *testing.T) {
	g := garage(t)
	g.Token = "wrong"
	if _, err := bucketcatalog.Apply(t.Context(), g, store{}, bucketcatalog.Catalog{Keys: []bucketcatalog.Key{keyOf("edge", "edge-assets")}}); err == nil {
		t.Fatal("applied")
	}
}
