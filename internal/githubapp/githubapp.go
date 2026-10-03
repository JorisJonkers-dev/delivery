// Package githubapp reads and writes one file of one repository as a GitHub App installation:
// the Collector's whole reach outside the cluster.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// File is one file on one branch of one repository, written as an App installation.
type File struct {
	// API is GitHub's API root, https://api.github.com.
	API string
	// Repository is owner/name.
	Repository string
	Path       string
	Branch     string

	AppID          string
	InstallationID string
	Key            *rsa.PrivateKey

	HTTP *http.Client
	Now  func() time.Time
}

// ParseKey reads the App's private key, in either PEM form GitHub and OpenSSL write.
func ParseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("the App key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read the App key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the App key is not an RSA key")
	}
	return key, nil
}

// Snapshot returns the file's content and the blob it was read at.
func (f File) Snapshot(ctx context.Context) ([]byte, string, error) {
	token, err := f.token(ctx)
	if err != nil {
		return nil, "", err
	}
	var content struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
	}
	target := f.contents() + "?ref=" + url.QueryEscape(f.Branch)
	if err := f.do(ctx, http.MethodGet, target, "Bearer "+token, nil, http.StatusOK, &content); err != nil {
		return nil, "", fmt.Errorf("read %s: %w", f.Path, err)
	}
	if content.Encoding != "base64" {
		return nil, "", fmt.Errorf("read %s: GitHub answered in %q, not base64", f.Path, content.Encoding)
	}
	document, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", f.Path, err)
	}
	return document, content.SHA, nil
}

// Commit replaces the file read at revision. GitHub refuses it when the file moved since, so two
// writers never overwrite one another.
func (f File) Commit(ctx context.Context, document []byte, revision, message string) error {
	token, err := f.token(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(document),
		"sha":     revision,
		"branch":  f.Branch,
	})
	if err != nil {
		return err
	}
	if err := f.do(ctx, http.MethodPut, f.contents(), "Bearer "+token, body, http.StatusOK, nil); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	return nil
}

func (f File) contents() string {
	return fmt.Sprintf("%s/repos/%s/contents/%s", strings.TrimRight(f.API, "/"), f.Repository, f.Path)
}

// token trades a short-lived App assertion for an installation token.
func (f File) token(ctx context.Context) (string, error) {
	assertion, err := f.assertion()
	if err != nil {
		return "", err
	}
	var granted struct {
		Token string `json:"token"`
	}
	target := fmt.Sprintf("%s/app/installations/%s/access_tokens", strings.TrimRight(f.API, "/"), url.PathEscape(f.InstallationID))
	if err := f.do(ctx, http.MethodPost, target, "Bearer "+assertion, nil, http.StatusCreated, &granted); err != nil {
		return "", fmt.Errorf("sign in as the App: %w", err)
	}
	if granted.Token == "" {
		return "", errors.New("sign in as the App: no token in the response")
	}
	return granted.Token, nil
}

// assertion is the RS256 JWT GitHub accepts from an App: issued a minute ago, to allow for a
// clock that runs ahead, and good for nine minutes, under GitHub's limit of ten.
func (f File) assertion() (string, error) {
	now := f.Now()
	header := encode(map[string]any{"alg": "RS256", "typ": "JWT"})
	claims := encode(map[string]any{
		"iss": f.AppID,
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
	})
	digest := sha256.Sum256([]byte(header + "." + claims))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign the App assertion: %w", err)
	}
	return header + "." + claims + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func encode(value map[string]any) string {
	// A map of strings and numbers always marshals.
	raw, _ := json.Marshal(value) //nolint:errchkjson // see above.
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (f File) do(ctx context.Context, method, target, authorization string, body []byte, want int, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	// A commit that creates nothing new still answers 200; one that creates the file answers 201.
	if res.StatusCode != want && (method != http.MethodPut || res.StatusCode != http.StatusCreated) {
		return errors.New(res.Status)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(raw, into)
}
