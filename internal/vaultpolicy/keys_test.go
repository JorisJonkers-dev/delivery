package vaultpolicy_test

import (
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/vaultpolicy"
)

const tribeltKeys = `{"keys":[` +
	`{"path":"secret/data/tribelt-system/app","key":"session_key","bytes":32,"encoding":"hex"},` +
	`{"path":"secret/data/tribelt-system/app","key":"visitor_hmac_key","bytes":24,"encoding":"base64url"}]}`

func TestTheGeneratedKeysAreReadInTheirOwnNamespace(t *testing.T) {
	keys, err := vaultpolicy.ReadKeys(rendered(t, map[string]string{"tribelt-system.generated.json": tribeltKeys}))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Key != "session_key" || keys[1].Encoding != "base64url" {
		t.Fatalf("keys = %+v", keys)
	}
	// The policies and roles beside them still read, and the keys do not count as either.
	if _, err := vaultpolicy.Read(rendered(t, map[string]string{"tribelt-system.generated.json": tribeltKeys})); err != nil {
		t.Fatal(err)
	}

	for name, text := range map[string]string{
		"a path in another namespace":     `{"keys":[{"path":"secret/data/auth-system/app","key":"k","bytes":32,"encoding":"hex"}]}`,
		"the namespace itself":            `{"keys":[{"path":"secret/data/tribelt-system","key":"k","bytes":32,"encoding":"hex"}]}`,
		"an encoding not offered":         `{"keys":[{"path":"secret/data/tribelt-system/app","key":"k","bytes":32,"encoding":"ascii"}]}`,
		"too few bytes":                   `{"keys":[{"path":"secret/data/tribelt-system/app","key":"k","bytes":8,"encoding":"hex"}]}`,
		"too many bytes":                  `{"keys":[{"path":"secret/data/tribelt-system/app","key":"k","bytes":4096,"encoding":"hex"}]}`,
		"a key twice":                     `{"keys":[{"path":"secret/data/tribelt-system/app","key":"k","bytes":32,"encoding":"hex"},{"path":"secret/data/tribelt-system/app","key":"k","bytes":32,"encoding":"hex"}]}`,
		"a key with a slash":              `{"keys":[{"path":"secret/data/tribelt-system/app","key":"a/b","bytes":32,"encoding":"hex"}]}`,
		"a field the render never writes": `{"keys":[{"path":"secret/data/tribelt-system/app","key":"k","bytes":32,"encoding":"hex","value":"x"}]}`,
	} {
		if _, err := vaultpolicy.ReadKeys(rendered(t, map[string]string{"tribelt-system.generated.json": text})); err == nil {
			t.Errorf("%s was read", name)
		}
	}
	if _, err := vaultpolicy.ReadKeys(rendered(t, map[string]string{"tribelt.generated.json": tribeltKeys})); err == nil {
		t.Error("a file named for no namespace was read")
	}
}

func TestARunWritesOnlyTheGeneratedKeysVaultLacks(t *testing.T) {
	f := newFakeVault(t)
	f.kv["tribelt-system/app"] = kvEntry{data: map[string]any{"visitor_hmac_key": "kept", "other": map[string]any{"nested": float64(1)}}, version: 3}
	keys, err := vaultpolicy.ReadKeys(rendered(t, map[string]string{"tribelt-system.generated.json": tribeltKeys}))
	if err != nil {
		t.Fatal(err)
	}

	written, err := vaultpolicy.ApplyKeys(t.Context(), f.vault(), keys)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(written, []string{"key secret/data/tribelt-system/app#session_key"}) {
		t.Fatalf("written = %v", written)
	}
	held := f.kv["tribelt-system/app"]
	if held.data["visitor_hmac_key"] != "kept" || held.data["other"].(map[string]any)["nested"] != float64(1) {
		t.Fatalf("a key Vault held changed: %v", held.data)
	}
	if raw, err := hex.DecodeString(held.data["session_key"].(string)); err != nil || len(raw) != 32 {
		t.Fatalf("session_key = %q", held.data["session_key"])
	}

	// Run again: everything is there, so nothing is written.
	f.writes = nil
	again, err := vaultpolicy.ApplyKeys(t.Context(), f.vault(), keys)
	if err != nil || len(again) != 0 || len(f.writes) != 0 {
		t.Fatalf("second run wrote %v %v, %v", again, f.writes, err)
	}

	// A path Vault does not hold at all is created, and a key in base64url has no padding.
	delete(f.kv, "tribelt-system/app")
	if _, err := vaultpolicy.ApplyKeys(t.Context(), f.vault(), keys); err != nil {
		t.Fatal(err)
	}
	if v := f.kv["tribelt-system/app"].data["visitor_hmac_key"].(string); len(v) != 32 || strings.ContainsAny(v, "=+/") {
		t.Fatalf("visitor_hmac_key = %q", v)
	}
}

func TestAKeyWrittenBetweenTheReadAndTheWriteIsNeverOverwritten(t *testing.T) {
	f := newFakeVault(t)
	f.broken = ""
	keys, err := vaultpolicy.ReadKeys(rendered(t, map[string]string{"tribelt-system.generated.json": tribeltKeys}))
	if err != nil {
		t.Fatal(err)
	}
	// Someone else writes the path after the job read it: the check-and-set write is refused,
	// and the run fails rather than replacing what they wrote.
	f.raceKV = func() {
		f.kv["tribelt-system/app"] = kvEntry{data: map[string]any{"session_key": "theirs"}, version: 1}
	}
	if _, err := vaultpolicy.ApplyKeys(t.Context(), f.vault(), keys); err == nil {
		t.Fatal("the run overwrote a key written between its read and its write")
	}
	if f.kv["tribelt-system/app"].data["session_key"] != "theirs" {
		t.Fatalf("Vault holds %v", f.kv["tribelt-system/app"].data)
	}
}

const adoptingKeys = `{"keys":[` +
	`{"path":"secret/data/tribelt-system/keys","key":"session_key","bytes":32,"encoding":"hex",` +
	`"adopt":{"path":"secret/data/platform/tribelt","key":"session_key"}},` +
	`{"path":"secret/data/tribelt-system/keys","key":"visitor_hmac_key","bytes":32,"encoding":"hex",` +
	`"adopt":{"path":"secret/data/platform/tribelt","key":"visitor_hmac_key"}}]}`

func TestAnAdoptedKeyStartsFromTheValueItsOldLocationHolds(t *testing.T) {
	f := newFakeVault(t)
	f.kv["platform/tribelt"] = kvEntry{data: map[string]any{"session_key": "sealed-with-this", "oidc.client_secret": "s"}, version: 7}
	keys, err := vaultpolicy.ReadKeys(rendered(t, map[string]string{"tribelt-system.generated.json": adoptingKeys}))
	if err != nil {
		t.Fatal(err)
	}

	written, err := vaultpolicy.ApplyKeys(t.Context(), f.vault(), keys)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(written, []string{
		"key secret/data/tribelt-system/keys#session_key (adopted from secret/data/platform/tribelt#session_key)",
		"key secret/data/tribelt-system/keys#visitor_hmac_key",
	}) {
		t.Fatalf("written = %v", written)
	}
	held := f.kv["tribelt-system/keys"].data
	if held["session_key"] != "sealed-with-this" {
		t.Fatalf("session_key = %v", held["session_key"])
	}
	// The old location holds no visitor key, so one is generated.
	if raw, err := hex.DecodeString(held["visitor_hmac_key"].(string)); err != nil || len(raw) != 32 {
		t.Fatalf("visitor_hmac_key = %v", held["visitor_hmac_key"])
	}
	if f.kv["platform/tribelt"].version != 7 {
		t.Fatal("the old location was written")
	}

	// Once held, the key is never replaced, whatever the old location holds now.
	f.kv["platform/tribelt"] = kvEntry{data: map[string]any{"session_key": "changed"}, version: 8}
	if again, err := vaultpolicy.ApplyKeys(t.Context(), f.vault(), keys); err != nil || len(again) != 0 {
		t.Fatalf("second run wrote %v, %v", again, err)
	}
	if f.kv["tribelt-system/keys"].data["session_key"] != "sealed-with-this" {
		t.Fatal("an adopted key was replaced")
	}
}

func TestAnAdoptedLocationIsOneKvPathAndOneKey(t *testing.T) {
	for name, adopt := range map[string]string{
		"a path outside the kv mount":     `{"path":"sys/policies/acl/root","key":"policy"}`,
		"a path that climbs out":          `{"path":"secret/data/../sys/policies/acl/root","key":"policy"}`,
		"an empty segment":                `{"path":"secret/data/platform//tribelt","key":"k"}`,
		"a key with a slash":              `{"path":"secret/data/platform/tribelt","key":"a/b"}`,
		"no key":                          `{"path":"secret/data/platform/tribelt"}`,
		"a field the render never writes": `{"path":"secret/data/platform/tribelt","key":"k","version":2}`,
	} {
		text := `{"keys":[{"path":"secret/data/tribelt-system/keys","key":"k","bytes":32,"encoding":"hex","adopt":` + adopt + `}]}`
		if _, err := vaultpolicy.ReadKeys(rendered(t, map[string]string{"tribelt-system.generated.json": text})); err == nil {
			t.Errorf("%s was read", name)
		}
	}
}
