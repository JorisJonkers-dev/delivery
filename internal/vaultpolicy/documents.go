// Package vaultpolicy writes the rendered Vault policies and Kubernetes auth roles into Vault
// (deploy-kit spec/v1/30-deliverables.md#vault-configuration-is-rendered-not-applied).
//
// The render writes two documents per identity that holds a grant,
// `<namespace>-<identity>.policy.json` and `<namespace>-<identity>.role.json`, and the Reconcile
// Unit hands them to this job as files. The job writes each under its file's name, leaves one
// that is already as rendered alone, and never deletes: what Vault holds and the render no
// longer names is reported for a human to remove.
package vaultpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	policySuffix = ".policy.json"
	roleSuffix   = ".role.json"
)

// name is what a document may be called: it becomes a path in Vault's API.
var name = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`) //nolint:gochecknoglobals // a compiled constant

// mounts are the paths a grant can derive (deploy-kit spec/v1/10-project-intent.md#secrets): a kv
// document and its metadata, a database credential, a transit key. A policy on anything else,
// Vault's own `sys/` and `auth/` above all, is not one the render writes.
var mounts = []string{"secret/data/", "secret/metadata/", "database/creds/", "transit/"} //nolint:gochecknoglobals // a constant list

// Role is a Kubernetes auth role as the render writes it: one ServiceAccount of one namespace,
// bound to the one policy of the same name.
type Role struct {
	ServiceAccounts []string `json:"bound_service_account_names"`
	Namespaces      []string `json:"bound_service_account_namespaces"`
	Policies        []string `json:"token_policies"`
}

// Document is one identity's policy and role, under the name Vault holds both by.
type Document struct {
	Name string
	// Policy is the policy's text, as rendered.
	Policy string
	Role   Role
}

// Read reads every identity's two documents from dir, by name. A directory that holds anything
// it cannot write as rendered is refused whole: half a render is not applied.
func Read(dir string) ([]Document, error) {
	policies, roles, err := load(dir)
	if err != nil {
		return nil, err
	}
	for identity := range roles {
		if _, paired := policies[identity]; !paired {
			return nil, fmt.Errorf("vaultpolicy: %s has a role and no policy", identity)
		}
	}
	names := make([]string, 0, len(policies))
	for identity := range policies {
		names = append(names, identity)
	}
	slices.Sort(names)
	documents := make([]Document, 0, len(names))
	for _, identity := range names {
		role, paired := roles[identity]
		if !paired {
			return nil, fmt.Errorf("vaultpolicy: %s has a policy and no role", identity)
		}
		document := Document{Name: identity, Policy: policies[identity], Role: role}
		if err := document.check(); err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	return documents, nil
}

// load reads the directory's files into the policies' texts and the roles, by the name before
// each file's suffix.
func load(dir string) (policies map[string]string, roles map[string]Role, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("vaultpolicy: read %s: %w", dir, err)
	}
	policies, roles = map[string]string{}, map[string]Role{}
	for _, entry := range entries {
		file := entry.Name()
		// A mounted ConfigMap carries its own bookkeeping beside the files, in entries named `..`.
		if strings.HasPrefix(file, "..") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // G304: the directory is the job's own input, and the name is one it listed
		if err != nil {
			return nil, nil, fmt.Errorf("vaultpolicy: read %s: %w", file, err)
		}
		switch {
		case strings.HasSuffix(file, policySuffix):
			policies[strings.TrimSuffix(file, policySuffix)] = string(raw)
		case strings.HasSuffix(file, roleSuffix):
			var role Role
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&role); err != nil {
				return nil, nil, fmt.Errorf("vaultpolicy: %s is not a role the render writes: %w", file, err)
			}
			roles[strings.TrimSuffix(file, roleSuffix)] = role
		default:
			return nil, nil, fmt.Errorf("vaultpolicy: %s is neither a policy nor a role", file)
		}
	}
	return policies, roles, nil
}

var errNotRendered = errors.New("is not a document the render writes")

// check holds a document to the shape the render gives it, so nothing this job writes can be
// more than a grant derives: the role binds the one ServiceAccount the name is for to the one
// policy of that name, and the policy grants only where a grant can.
func (d Document) check() error {
	if !name.MatchString(d.Name) {
		return fmt.Errorf("vaultpolicy: the name %q %w", d.Name, errNotRendered)
	}
	r := d.Role
	if len(r.ServiceAccounts) != 1 || len(r.Namespaces) != 1 || d.Name != r.Namespaces[0]+"-"+r.ServiceAccounts[0] {
		return fmt.Errorf("vaultpolicy: the role of %s binds another identity than its name: %w", d.Name, errNotRendered)
	}
	if len(r.Policies) != 1 || r.Policies[0] != d.Name {
		return fmt.Errorf("vaultpolicy: the role of %s binds another policy than its own: %w", d.Name, errNotRendered)
	}
	var policy struct {
		Path map[string]struct {
			Capabilities []string `json:"capabilities"`
		} `json:"path"`
	}
	decoder := json.NewDecoder(strings.NewReader(d.Policy))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return fmt.Errorf("vaultpolicy: the policy of %s does not parse: %w", d.Name, err)
	}
	for path := range policy.Path {
		granted := slices.ContainsFunc(mounts, func(mount string) bool { return strings.HasPrefix(path, mount) })
		if !granted || strings.Contains(path, "..") || strings.ContainsAny(path, "*+") {
			return fmt.Errorf("vaultpolicy: the policy of %s grants %q: %w", d.Name, path, errNotRendered)
		}
	}
	return nil
}
