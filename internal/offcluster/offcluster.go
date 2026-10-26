// Package offcluster copies a backup generation off the cluster, the copy an `irreplaceable`
// volume's class asks for (deploy-kit#470): encrypted on the pod to an age recipient, so the
// provider never holds a readable copy, written to an S3-compatible bucket, read back, and pruned
// to the class's retain count.
//
// What the render hands the method: BACKUP_OFF_CLUSTER, the destination as s3://bucket[/prefix];
// BACKUP_OFF_CLUSTER_PATH, where under it this volume's copies go; and
// BACKUP_OFF_CLUSTER_RECIPIENT, the age public key the Platform document names. The credential the
// operator writes at the destination's Secret Store path carries AWS_ENDPOINT_URL,
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, and AWS_REGION where the provider wants one.
package offcluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
)

// Extension ends the name of every copy this package writes: the local name, encrypted.
const Extension = ".age"

// maxObject is the most one PUT may write; a larger copy needs a multipart upload, not built.
const maxObject = 5 << 30

// Settings say where copies go, who may read them, and the keys that write them.
type Settings struct {
	Endpoint    *url.URL
	Region      string
	Bucket      string
	Prefix      string
	Recipient   age.Recipient
	Credentials Credentials
}

// ReadSettings reads the destination and the variables beside it, and refuses any that is
// missing: an off-cluster plan the method cannot carry out fails rather than guess.
func ReadSettings(getenv func(string) string) (Settings, error) {
	var missing []string
	for _, name := range []string{
		"BACKUP_OFF_CLUSTER", "BACKUP_OFF_CLUSTER_PATH", "BACKUP_OFF_CLUSTER_RECIPIENT",
		"AWS_ENDPOINT_URL", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
	} {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Settings{}, fmt.Errorf("%v not set: an off-cluster copy needs its destination, its recipient and its credential's keys", missing)
	}
	destination, err := url.Parse(getenv("BACKUP_OFF_CLUSTER"))
	if err != nil || destination.Scheme != "s3" || destination.Host == "" {
		return Settings{}, fmt.Errorf("BACKUP_OFF_CLUSTER %q is not s3://bucket[/prefix]", getenv("BACKUP_OFF_CLUSTER"))
	}
	endpoint, err := url.Parse(getenv("AWS_ENDPOINT_URL"))
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" {
		return Settings{}, fmt.Errorf("AWS_ENDPOINT_URL %q is not an http(s) URL", getenv("AWS_ENDPOINT_URL"))
	}
	recipient, err := age.ParseX25519Recipient(getenv("BACKUP_OFF_CLUSTER_RECIPIENT"))
	if err != nil {
		return Settings{}, fmt.Errorf("BACKUP_OFF_CLUSTER_RECIPIENT is not an age public key: %w", err)
	}
	region := getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	prefix := strings.Trim(path.Join(destination.Path, getenv("BACKUP_OFF_CLUSTER_PATH")), "/")
	return Settings{
		Endpoint:    endpoint,
		Region:      region,
		Bucket:      destination.Host,
		Prefix:      prefix,
		Recipient:   recipient,
		Credentials: Credentials{AccessKey: getenv("AWS_ACCESS_KEY_ID"), SecretKey: getenv("AWS_SECRET_ACCESS_KEY")},
	}, nil
}

// Store is one volume's place in the bucket.
type Store struct {
	Settings
	HTTP *http.Client
	Now  func() time.Time
}

// Copy encrypts the generation at local into scratch, writes it to the bucket under its own name
// plus Extension, and reads it back: an object whose size or SHA-256 differs from what was sent
// fails the copy. It returns the object's key.
func (s Store) Copy(ctx context.Context, local, scratch string) (string, error) {
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	name := filepath.Base(local) + Extension
	sealed := filepath.Join(scratch, name)
	size, sum, err := s.seal(local, sealed)
	if err != nil {
		return "", err
	}
	if size > maxObject {
		return "", fmt.Errorf("%s is %d bytes, over the %d one upload may write", name, size, int64(maxObject))
	}
	key := s.key(name)
	body, err := os.Open(sealed) //nolint:gosec // G304: the file this method just wrote
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	put, err := s.request(ctx, http.MethodPut, key, nil, body)
	if err != nil {
		return "", err
	}
	put.ContentLength = size
	put.Header.Set("Content-Type", "application/octet-stream")
	put.Header.Set("X-Amz-Meta-Sha256", sum)
	// The signed payload hash is checked by the provider: a body that changed on the way is refused.
	if _, err := s.do(put, sum); err != nil {
		return "", fmt.Errorf("write %s: %w", key, err)
	}
	return key, s.verify(ctx, key, size, sum)
}

// seal writes local, encrypted to the recipient, to sealed, and returns its size and SHA-256.
func (s Store) seal(local, sealed string) (int64, string, error) {
	source, err := os.Open(local) //nolint:gosec // G304: the generation this run wrote
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(sealed, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the method's own scratch directory
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = target.Close() }()
	digest := sha256.New()
	counted := &counter{}
	encrypted, err := age.Encrypt(io.MultiWriter(target, digest, counted), s.Recipient)
	if err != nil {
		return 0, "", err
	}
	if _, err := io.Copy(encrypted, source); err != nil {
		return 0, "", fmt.Errorf("encrypt %s: %w", local, err)
	}
	if err := encrypted.Close(); err != nil {
		return 0, "", err
	}
	if err := target.Close(); err != nil {
		return 0, "", err
	}
	return counted.n, hexOf(digest), nil
}

// verify reads the object's size and the SHA-256 it was written with back from the bucket.
func (s Store) verify(ctx context.Context, key string, size int64, sum string) error {
	head, err := s.request(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return err
	}
	res, err := s.do(head, emptyHash)
	if err != nil {
		return fmt.Errorf("read back %s: %w", key, err)
	}
	if res.ContentLength != size || res.Header.Get("X-Amz-Meta-Sha256") != sum {
		return fmt.Errorf("read back %s: %d bytes with SHA-256 %q, wrote %d with %q",
			key, res.ContentLength, res.Header.Get("X-Amz-Meta-Sha256"), size, sum)
	}
	return nil
}

// Prune deletes all but the newest retain copies under the volume's prefix that keep accepts by
// name, and returns their keys. A name orders as its time does, so the newest sort last.
func (s Store) Prune(ctx context.Context, keep func(name string) bool, retain int) ([]string, error) {
	keys, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	var copies []string
	for _, key := range keys {
		if name := strings.TrimPrefix(key, s.Prefix+"/"); !strings.Contains(name, "/") && keep(name) {
			copies = append(copies, key)
		}
	}
	sort.Strings(copies)
	if len(copies) <= retain {
		return nil, nil
	}
	removed := copies[:len(copies)-retain]
	for _, key := range removed {
		req, err := s.request(ctx, http.MethodDelete, key, nil, nil)
		if err != nil {
			return nil, err
		}
		if _, err := s.do(req, emptyHash); err != nil {
			return nil, fmt.Errorf("delete %s: %w", key, err)
		}
	}
	return removed, nil
}

type listing struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// list is every key under the volume's prefix, across as many pages as the bucket returns.
func (s Store) list(ctx context.Context) ([]string, error) {
	var keys []string
	token := ""
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {s.Prefix + "/"}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		req, err := s.request(ctx, http.MethodGet, "", query, nil)
		if err != nil {
			return nil, err
		}
		res, err := s.do(req, emptyHash)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", s.Prefix, err)
		}
		var page listing
		if err := xml.Unmarshal(res.body, &page); err != nil {
			return nil, fmt.Errorf("list %s: %w", s.Prefix, err)
		}
		for _, object := range page.Contents {
			keys = append(keys, object.Key)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return keys, nil
		}
		token = page.NextContinuationToken
	}
}

func (s Store) key(name string) string {
	return s.Prefix + "/" + name
}

// request builds a path-style request for key in the bucket, or for the bucket itself.
func (s Store) request(ctx context.Context, method, key string, query url.Values, body io.Reader) (*http.Request, error) {
	target := *s.Endpoint
	target.Path = "/" + s.Bucket
	if key != "" {
		target.Path += "/" + key
	}
	target.RawPath = ""
	target.RawQuery = query.Encode()
	return http.NewRequestWithContext(ctx, method, target.String(), body)
}

type response struct {
	ContentLength int64
	Header        http.Header
	body          []byte
}

// errStatus is a response outside 2xx, with what the provider said about it.
var errStatus = errors.New("the bucket refused the request")

// send signs and sends req, and returns the response for its caller to read and close, or what
// the provider said where it refused.
func (s Store) send(req *http.Request, payloadHash string) (*http.Response, error) {
	sign(req, payloadHash, s.Region, s.Credentials, s.Now())
	res, err := s.HTTP.Do(req) //nolint:gosec // G704: the endpoint is the platform's, from the credential the operator writes
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		said, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		_ = res.Body.Close()
		return nil, fmt.Errorf("%w: %s %s", errStatus, res.Status, bytes.TrimSpace(said))
	}
	return res, nil
}

func (s Store) do(req *http.Request, payloadHash string) (response, error) {
	res, err := s.send(req, payloadHash)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<24))
	if err != nil {
		return response{}, err
	}
	return response{ContentLength: res.ContentLength, Header: res.Header, body: body}, nil
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func hexOf(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil))
}
