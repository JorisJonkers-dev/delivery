package catalog

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Writer writes one Vault document.
type Writer interface {
	Write(ctx context.Context, path string, body any) error
}

const (
	defaultTTL = "1h"
	maxTTL     = "24h"
)

// WriteRoles writes, for each database, the two Secret Store database roles
// whose logins act as its owner and data roles, and says which it wrote.
func WriteRoles(ctx context.Context, vault Writer, c Catalog) ([]string, error) {
	var written []string
	for _, d := range c.Databases {
		for _, group := range []string{d.Owner, d.Data} {
			path := "database/roles/" + group
			if err := vault.Write(ctx, path, roleOf(c.Connection, group)); err != nil {
				return written, fmt.Errorf("catalog: write %s: %w", path, err)
			}
			written = append(written, path)
		}
	}
	return written, nil
}

// roleOf is a database role whose every login is a member of group and acts as
// it on sign-in, so what the login creates belongs to the group.
func roleOf(connection, group string) map[string]any {
	quoted := pgx.Identifier{group}.Sanitize()
	return map[string]any{
		"db_name": connection,
		"creation_statements": []string{
			`CREATE ROLE "{{name}}" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}' IN ROLE ` + quoted + ` INHERIT;`,
			`ALTER ROLE "{{name}}" SET role = ` + quoted + `;`,
		},
		"default_ttl": defaultTTL,
		"max_ttl":     maxTTL,
	}
}
