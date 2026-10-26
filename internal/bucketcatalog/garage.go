package bucketcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxBody bounds what is read of an admin API response.
const maxBody = 1 << 20

// Garage is the part of Garage's admin API v2 the method uses, signed in with one token.
type Garage struct {
	// Address is the admin API's, without a trailing slash.
	Address string
	Token   string
	HTTP    *http.Client
}

// Credentials is a key's id and secret.
type Credentials struct {
	ID     string `json:"accessKeyId"`
	Secret string `json:"secretAccessKey"`
}

var errStatus = errors.New("garage answered")

// call sends one request and decodes a JSON answer into out. A 404 is reported as found false,
// which is how Garage says a key or a bucket is not there.
func (g Garage) call(ctx context.Context, method, path string, query url.Values, body, out any) (found bool, err error) {
	var sent io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return false, err
		}
		sent = bytes.NewReader(encoded)
	}
	target := strings.TrimSuffix(g.Address, "/") + "/v2/" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, sent)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := g.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return false, err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return false, nil
	case res.StatusCode >= http.StatusMultipleChoices:
		// Garage's error text names keys and buckets by id, never a secret.
		return false, fmt.Errorf("%s %s: %w %d: %s", method, path, errStatus, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return false, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return true, nil
}

// KeyIDs returns the ids of every key the store holds under name.
func (g Garage) KeyIDs(ctx context.Context, name string) ([]string, error) {
	var keys []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if _, err := g.call(ctx, http.MethodGet, "ListKeys", nil, nil, &keys); err != nil {
		return nil, err
	}
	var ids []string
	for _, k := range keys {
		if k.Name == name {
			ids = append(ids, k.ID)
		}
	}
	return ids, nil
}

// Key returns a key the store holds, with its secret, and found false where it holds none by id.
func (g Garage) Key(ctx context.Context, id string) (Credentials, bool, error) {
	var key Credentials
	found, err := g.call(ctx, http.MethodGet, "GetKeyInfo", url.Values{"id": {id}, "showSecretKey": {"true"}}, nil, &key)
	return key, found, err
}

// ImportKey adds a key the store does not hold, under name.
func (g Garage) ImportKey(ctx context.Context, name string, key Credentials) error {
	_, err := g.call(ctx, http.MethodPost, "ImportKey", nil, map[string]string{
		"name": name, "accessKeyId": key.ID, "secretAccessKey": key.Secret,
	}, nil)
	return err
}

// CreateKey mints a key under name.
func (g Garage) CreateKey(ctx context.Context, name string) (Credentials, error) {
	var key Credentials
	_, err := g.call(ctx, http.MethodPost, "CreateKey", nil, map[string]string{"name": name}, &key)
	return key, err
}

// Bucket returns the id of the bucket the store holds under alias, creating it where it holds
// none, and whether it created it.
func (g Garage) Bucket(ctx context.Context, alias string) (string, bool, error) {
	var bucket struct {
		ID string `json:"id"`
	}
	found, err := g.call(ctx, http.MethodGet, "GetBucketInfo", url.Values{"globalAlias": {alias}}, nil, &bucket)
	if err != nil || found {
		return bucket.ID, false, err
	}
	_, err = g.call(ctx, http.MethodPost, "CreateBucket", nil, map[string]string{"globalAlias": alias}, &bucket)
	return bucket.ID, err == nil, err
}

// Allow grants a key read and write on a bucket, and never ownership. Granting what the key
// already holds changes nothing.
func (g Garage) Allow(ctx context.Context, bucketID, keyID string) error {
	_, err := g.call(ctx, http.MethodPost, "AllowBucketKey", nil, map[string]any{
		"bucketId":    bucketID,
		"accessKeyId": keyID,
		"permissions": map[string]bool{"read": true, "write": true, "owner": false},
	}, nil)
	return err
}
