package vaultpolicy

import (
	"context"
	"fmt"
	"slices"
)

// Report is what one run did, by name.
type Report struct {
	// Written are the policies and roles the run wrote, each as `policy <name>` or `role <name>`.
	Written []string
	// Unchanged counts the documents Vault already held as rendered.
	Unchanged int
	// Stale are the names Vault holds in the render's own form that the render no longer
	// names. The job never deletes: these are for a human to remove.
	Stale []string
}

// rendered reports whether a name has the form the render gives one: `<project>-system-<identity>`.
// It is how the job tells what the render once wrote from the platform's own fixtures.
func rendered(name string) bool {
	_, _, named := identityOf(name)
	return named
}

// Apply writes every document Vault does not already hold as rendered: the policy first, so a
// role never names a policy that is not there. One that fails stops the run, so no Application
// starts against policies Vault does not hold.
func Apply(ctx context.Context, vault Vault, documents []Document) (Report, error) {
	var report Report
	names := map[string]bool{}
	for _, d := range documents {
		names[d.Name] = true
		for _, write := range []func(context.Context, Vault, Document) (string, error){writePolicy, writeRole} {
			wrote, err := write(ctx, vault, d)
			switch {
			case err != nil:
				return report, err
			case wrote == "":
				report.Unchanged++
			default:
				report.Written = append(report.Written, wrote)
			}
		}
	}
	stale, err := staleIn(ctx, vault, names)
	report.Stale = stale
	return report, err
}

// writePolicy writes the document's policy unless Vault holds it as rendered, and says what it wrote.
func writePolicy(ctx context.Context, vault Vault, d Document) (string, error) {
	held, found, err := vault.policy(ctx, d.Name)
	if err != nil {
		return "", fmt.Errorf("vaultpolicy: read the policy %s: %w", d.Name, err)
	}
	if found && held == d.Policy {
		return "", nil
	}
	if err := vault.putPolicy(ctx, d.Name, d.Policy); err != nil {
		return "", fmt.Errorf("vaultpolicy: write the policy %s: %w", d.Name, err)
	}
	return "policy " + d.Name, nil
}

// writeRole writes the document's role unless Vault holds it bound as rendered, and says what it wrote.
func writeRole(ctx context.Context, vault Vault, d Document) (string, error) {
	held, found, err := vault.role(ctx, d.Name)
	if err != nil {
		return "", fmt.Errorf("vaultpolicy: read the role %s: %w", d.Name, err)
	}
	same := slices.Equal(held.ServiceAccounts, d.Role.ServiceAccounts) && slices.Equal(held.Namespaces, d.Role.Namespaces) &&
		slices.Equal(held.Policies, d.Role.Policies) && held.NamespaceSelector == d.Role.NamespaceSelector
	if found && same {
		return "", nil
	}
	if err := vault.putRole(ctx, d.Name, d.Role); err != nil {
		return "", fmt.Errorf("vaultpolicy: write the role %s: %w", d.Name, err)
	}
	return "role " + d.Name, nil
}

// staleIn lists what Vault holds in the render's form under a name the render no longer has.
func staleIn(ctx context.Context, vault Vault, names map[string]bool) ([]string, error) {
	var stale []string
	for _, listed := range []struct{ kind, path string }{{"policy", "sys/policies/acl"}, {"role", authMount + "/role"}} {
		held, err := vault.list(ctx, listed.path)
		if err != nil {
			return nil, fmt.Errorf("vaultpolicy: list what Vault holds: %w", err)
		}
		for _, name := range held {
			if rendered(name) && !names[name] {
				stale = append(stale, listed.kind+" "+name)
			}
		}
	}
	slices.Sort(stale)
	return stale, nil
}
