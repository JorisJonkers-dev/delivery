package vaultpolicy

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

// authMount is the Kubernetes auth method's mount, a platform fixture
// (deploy-kit spec/v1/30-deliverables.md#vault-configuration-is-rendered-not-applied).
const authMount = "auth/kubernetes"

// maxBody bounds what is read of a Vault response.
const maxBody = 1 << 20

// Vault is the part of Vault's HTTP API the job uses, as one token.
type Vault struct {
	// Address is Vault's, without a trailing slash.
	Address string
	Token   string
	HTTP    *http.Client
}

var errStatus = errors.New("vault answered")

// call sends one request and decodes a JSON answer into out. A 404 is reported as found false,
// which is how Vault says a policy, a role or an empty list is not there.
func (v Vault) call(ctx context.Context, method, path string, body, out any) (found bool, err error) {
	var sent io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return false, err
		}
		sent = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(v.Address, "/")+"/v1/"+path, sent)
	if err != nil {
		return false, err
	}
	if v.Token != "" {
		req.Header.Set("X-Vault-Token", v.Token)
	}
	res, err := v.HTTP.Do(req)
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
		// Vault's error text names paths and never a secret; it is what an operator needs.
		return false, fmt.Errorf("%s %s: %w %d: %s", method, path, errStatus, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return false, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return true, nil
}

// Login trades a ServiceAccount token for a Vault token, as role.
func Login(ctx context.Context, client *http.Client, address, role, jwt string) (Vault, error) {
	var answer struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	anonymous := Vault{Address: address, HTTP: client}
	found, err := anonymous.call(ctx, http.MethodPost, authMount+"/login", map[string]string{"role": role, "jwt": jwt}, &answer)
	if err != nil {
		return Vault{}, fmt.Errorf("vaultpolicy: log in as %s: %w", role, err)
	}
	if !found || answer.Auth.ClientToken == "" {
		return Vault{}, fmt.Errorf("vaultpolicy: log in as %s: Vault gave no token", role)
	}
	return Vault{Address: address, Token: answer.Auth.ClientToken, HTTP: client}, nil
}

func (v Vault) policy(ctx context.Context, name string) (string, bool, error) {
	var answer struct {
		Data struct {
			Policy string `json:"policy"`
		} `json:"data"`
	}
	found, err := v.call(ctx, http.MethodGet, "sys/policies/acl/"+url.PathEscape(name), nil, &answer)
	return answer.Data.Policy, found, err
}

func (v Vault) putPolicy(ctx context.Context, name, policy string) error {
	_, err := v.call(ctx, http.MethodPut, "sys/policies/acl/"+url.PathEscape(name), map[string]string{"policy": policy}, nil)
	return err
}

func (v Vault) role(ctx context.Context, name string) (Role, bool, error) {
	var answer struct {
		Data Role `json:"data"`
	}
	found, err := v.call(ctx, http.MethodGet, authMount+"/role/"+url.PathEscape(name), nil, &answer)
	return answer.Data, found, err
}

func (v Vault) putRole(ctx context.Context, name string, role Role) error {
	_, err := v.call(ctx, http.MethodPost, authMount+"/role/"+url.PathEscape(name), role, nil)
	return err
}

// list returns the keys under a path Vault lists, and none where it holds nothing there.
func (v Vault) list(ctx context.Context, path string) ([]string, error) {
	var answer struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	_, err := v.call(ctx, "LIST", path, nil, &answer)
	return answer.Data.Keys, err
}

// kv reads a KV version 2 document as Vault holds it, with the version a check-and-set write names;
// a document Vault does not hold is version 0.
func (v Vault) kv(ctx context.Context, path string) (map[string]json.RawMessage, int, error) {
	var answer struct {
		Data struct {
			Data     map[string]json.RawMessage `json:"data"`
			Metadata struct {
				Version int `json:"version"`
			} `json:"metadata"`
		} `json:"data"`
	}
	found, err := v.call(ctx, http.MethodGet, path, nil, &answer)
	if err != nil || !found {
		return nil, 0, err
	}
	return answer.Data.Data, answer.Data.Metadata.Version, nil
}

// putKV writes a KV version 2 document only if Vault still holds the version read.
func (v Vault) putKV(ctx context.Context, path string, data map[string]json.RawMessage, version int) error {
	_, err := v.call(ctx, http.MethodPost, path, map[string]any{"data": data, "options": map[string]int{"cas": version}}, nil)
	return err
}

// ReadKV reads a KV version 2 document's keys as Vault holds them; nil where it holds none.
func (v Vault) ReadKV(ctx context.Context, path string) (map[string]json.RawMessage, error) {
	data, _, err := v.kv(ctx, path)
	return data, err
}

// Write writes one document at path.
func (v Vault) Write(ctx context.Context, path string, body any) error {
	_, err := v.call(ctx, http.MethodPost, path, body, nil)
	return err
}
