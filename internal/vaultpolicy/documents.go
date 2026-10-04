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
	"io"
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

// label is one part of a name: a Project, or an identity within it.
const label = `[a-z0-9]([a-z0-9-]*[a-z0-9])?`

// part is what a Project's name and an identity's may each be.
var part = regexp.MustCompile(`^` + label + `$`) //nolint:gochecknoglobals // a compiled constant

// namespaceSuffix ends every namespace the render derives, a Project's own
// (deploy-kit spec/v1/16-dependencies.md#process-identity).
const namespaceSuffix = "-system"

// identityOf reads a document's name the one way the job reads it: the Project is what stands
// before the first `-system-`, and the identity is everything after. A name has one reading, so
// no second namespace and ServiceAccount can spell the name another identity's policy is under.
func identityOf(name string) (namespace, serviceAccount string, ok bool) {
	project, identity, found := strings.Cut(name, namespaceSuffix+"-")
	if !found || !part.MatchString(project) || !part.MatchString(identity) {
		return "", "", false
	}
	return project + namespaceSuffix, identity, true
}

// segment is one step of a path a grant names: no glob, no `+`, no template, no `..`, not empty.
const segment = `[A-Za-z0-9_][A-Za-z0-9_.-]*`

// grantable are the forms of path the render writes for a grant (deploy-kit
// spec/v1/10-project-intent.md#secrets), each with what it lets a grant do there: read a kv
// document and its metadata, read a database credential, run a transit operation on one key. A path of any other form, Vault's own `sys/` and `auth/`
// above all, or a capability the form does not carry, `sudo` above all, is not one the render
// writes. A form the render gains is refused until this list gains it.
var grantable = []struct { //nolint:gochecknoglobals // a constant list
	path         *regexp.Regexp
	capabilities []string
}{
	{regexp.MustCompile(`^secret/(data|metadata)/` + segment + `(/` + segment + `)*$`), []string{"read"}},
	{regexp.MustCompile(`^database/creds/` + segment + `$`), []string{"read"}},
	{regexp.MustCompile(`^transit/(sign|verify|encrypt|decrypt)/` + segment + `$`), []string{"update"}},
	{regexp.MustCompile(`^transit/keys/` + segment + `/rotate$`), []string{"update"}},
}

// Role is a Kubernetes auth role as the render writes it: one ServiceAccount of one namespace,
// bound to the one policy of the same name.
type Role struct {
	ServiceAccounts []string `json:"bound_service_account_names"`
	Namespaces      []string `json:"bound_service_account_namespaces"`
	Policies        []string `json:"token_policies"`
	// NamespaceSelector admits every namespace a label selects, beside the ones named. The
	// render never writes one, so the job writes it empty, which clears one Vault holds.
	NamespaceSelector string `json:"bound_service_account_namespace_selector"`
}

// Document is one identity's policy and role, under the name Vault holds both by.
type Document struct {
	Name string
	// Policy is the policy's text: the encoding of what the job read and checked, never the
	// file's own bytes, so Vault parses nothing the check did not.
	Policy string
	Role   Role
}

// rules is a policy as the render writes it: what may be done, by path.
type rules struct {
	Path map[string]struct {
		Capabilities []string `json:"capabilities"`
	} `json:"path"`
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
		document, err := checked(identity, policies[identity], role)
		if err != nil {
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
			if err := decode(string(raw), &role); err != nil {
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

// decode reads text as the one JSON value it is, with no field out does not have and nothing
// after it.
func decode(text string, out any) error {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("more than one value: %w", errNotRendered)
	}
	return nil
}

// checked holds a document to the shape the render gives it, so nothing this job writes can be
// more than a grant derives: the role binds the one ServiceAccount and namespace its name reads
// as, to the one policy of that name, and the policy grants only where a grant can and only
// what a grant may. The policy it returns is the encoding of what it checked.
func checked(identity, policy string, r Role) (Document, error) {
	namespace, serviceAccount, named := identityOf(identity)
	if !named {
		return Document{}, fmt.Errorf("vaultpolicy: the name %q %w", identity, errNotRendered)
	}
	if !slices.Equal(r.ServiceAccounts, []string{serviceAccount}) || !slices.Equal(r.Namespaces, []string{namespace}) || r.NamespaceSelector != "" {
		return Document{}, fmt.Errorf("vaultpolicy: the role of %s binds another identity than its name: %w", identity, errNotRendered)
	}
	if len(r.Policies) != 1 || r.Policies[0] != identity {
		return Document{}, fmt.Errorf("vaultpolicy: the role of %s binds another policy than its own: %w", identity, errNotRendered)
	}
	var read rules
	if err := decode(policy, &read); err != nil {
		return Document{}, fmt.Errorf("vaultpolicy: the policy of %s does not parse: %w", identity, err)
	}
	for path, rule := range read.Path {
		if !grants(path, rule.Capabilities) {
			return Document{}, fmt.Errorf("vaultpolicy: the policy of %s grants %v on %q: %w", identity, rule.Capabilities, path, errNotRendered)
		}
	}
	text, err := json.MarshalIndent(read, "", "  ")
	if err != nil {
		return Document{}, fmt.Errorf("vaultpolicy: the policy of %s: %w", identity, err)
	}
	return Document{Name: identity, Policy: string(text) + "\n", Role: r}, nil
}

// grants reports whether a grant can derive these capabilities on this path.
func grants(path string, capabilities []string) bool {
	if len(capabilities) == 0 {
		return false
	}
	for _, form := range grantable {
		if form.path.MatchString(path) {
			return !slices.ContainsFunc(capabilities, func(c string) bool { return !slices.Contains(form.capabilities, c) })
		}
	}
	return false
}
