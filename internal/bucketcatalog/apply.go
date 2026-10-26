package bucketcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// The keys of a key's document, as the consumer reads them (deploy-kit
// spec/v1/16-dependencies.md#the-bucket-catalog).
const (
	idKey     = "access-key-id"
	secretKey = "secret-access-key"
)

// Store is the part of the Secret Store the method uses: it reads a key's document, and writes
// one only where none is.
type Store interface {
	ReadKV(ctx context.Context, path string) (map[string]json.RawMessage, error)
	CreateKV(ctx context.Context, path string, data map[string]string) error
}

// Report says what a run changed: the keys it minted or imported, and the buckets it created.
type Report struct {
	Minted   []string
	Imported []string
	Buckets  []string
}

var (
	errHalfKey  = errors.New("holds one of the key's two halves")
	errTwoKeys  = errors.New("the store holds more than one key by that name")
	errMismatch = errors.New("the store holds a different secret for that key id")
)

// Apply applies the catalog, key by key: the key first, from the Secret Store or minted, then
// each of its buckets, then its grant on each. It never deletes a key, a bucket or a grant, and a
// second run changes nothing.
func Apply(ctx context.Context, garage Garage, store Store, c Catalog) (Report, error) {
	var report Report
	for _, k := range c.Keys {
		id, err := key(ctx, garage, store, k, &report)
		if err != nil {
			return report, fmt.Errorf("key %s: %w", k.Name, err)
		}
		for _, alias := range k.Buckets {
			bucket, created, err := garage.Bucket(ctx, alias)
			if err != nil {
				return report, fmt.Errorf("bucket %s: %w", alias, err)
			}
			if created {
				report.Buckets = append(report.Buckets, alias)
			}
			if err := garage.Allow(ctx, bucket, id); err != nil {
				return report, fmt.Errorf("grant %s on %s: %w", k.Name, alias, err)
			}
		}
	}
	return report, nil
}

// key makes the store and the Secret Store hold one key for k, and returns its id.
//
// A document already there wins: the store imports it where it lacks it, so a store rebuilt from
// nothing gets the same key back. Where there is no document, a key the store holds under the
// project's name is the one a run before this minted and could not write down, and is written
// now; only where there is neither is a key minted.
func key(ctx context.Context, garage Garage, store Store, k Key, report *Report) (string, error) {
	held, err := store.ReadKV(ctx, k.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", k.Path, err)
	}
	if held != nil {
		return imported(ctx, garage, k, held, report)
	}
	ids, err := garage.KeyIDs(ctx, k.Name)
	if err != nil {
		return "", err
	}
	var minted Credentials
	switch len(ids) {
	case 0:
		if minted, err = garage.CreateKey(ctx, k.Name); err != nil {
			return "", err
		}
	case 1:
		if minted, _, err = garage.Key(ctx, ids[0]); err != nil {
			return "", err
		}
	default:
		return "", errTwoKeys
	}
	if err := store.CreateKV(ctx, k.Path, map[string]string{idKey: minted.ID, secretKey: minted.Secret}); err != nil {
		return "", fmt.Errorf("write %s: %w", k.Path, err)
	}
	report.Minted = append(report.Minted, k.Name)
	return minted.ID, nil
}

// imported makes the store hold the key a document names.
func imported(ctx context.Context, garage Garage, k Key, held map[string]json.RawMessage, report *Report) (string, error) {
	var want Credentials
	if json.Unmarshal(held[idKey], &want.ID) != nil || json.Unmarshal(held[secretKey], &want.Secret) != nil ||
		want.ID == "" || want.Secret == "" {
		return "", fmt.Errorf("%s %w", k.Path, errHalfKey)
	}
	has, found, err := garage.Key(ctx, want.ID)
	switch {
	case err != nil:
		return "", err
	case found && has.Secret != want.Secret:
		return "", fmt.Errorf("%s: %w", want.ID, errMismatch)
	case found:
		return want.ID, nil
	}
	if err := garage.ImportKey(ctx, k.Name, want); err != nil {
		return "", err
	}
	report.Imported = append(report.Imported, k.Name)
	return want.ID, nil
}
