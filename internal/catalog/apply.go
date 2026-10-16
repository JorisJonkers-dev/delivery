package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Connect opens a connection to one database of the datastore, as the admin login.
type Connect func(ctx context.Context, database string) (*pgx.Conn, error)

// Report is what one run changed: each role and database it created, and each
// database it adopted, and from whom.
type Report struct {
	Created []string
	Adopted []string
}

var errUnadoptable = errors.New("is never adopted")

// unprivileged holds, of a role, what adopting it must not hand on: no attribute
// beyond owning, and no database but the one adopted. REASSIGN OWNED moves every
// database a role owns, and the role's members gain the owner role, so a role
// shared with another project is never adopted.
const unprivileged = `NOT (r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls)
	AND r.rolname NOT LIKE 'pg\_%' AND r.rolname NOT LIKE '%-owner' AND r.rolname NOT LIKE '%-data'
	AND NOT EXISTS (SELECT 1 FROM pg_database o WHERE o.datdba = r.oid AND o.datname <> $2)`

// Apply applies every database of c, in order, and stops at the first that fails.
// It never drops a role, a database or a privilege, so a run applied twice
// changes nothing the second time.
func Apply(ctx context.Context, connect Connect, c Catalog) (Report, error) {
	var report Report
	for _, d := range c.Databases {
		if err := apply(ctx, connect, d, &report); err != nil {
			return report, fmt.Errorf("catalog: %s: %w", d.Name, err)
		}
	}
	return report, nil
}

func apply(ctx context.Context, connect Connect, d Database, report *Report) error {
	server, err := connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = server.Close(context.Background()) }()
	if err := ensureRoles(ctx, server, d, report); err != nil {
		return err
	}
	owner, err := ensureDatabase(ctx, server, d)
	if err != nil {
		return err
	}
	if owner == "" {
		report.Created = append(report.Created, "database "+d.Name)
	}
	db, err := connect(ctx, d.Name)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close(context.Background()) }()
	if owner != "" && owner != d.Owner {
		if err := adoptable(ctx, server, owner, d.Name); err != nil {
			return err
		}
		if err := adopt(ctx, db, d, owner); err != nil {
			return err
		}
		report.Adopted = append(report.Adopted, d.Name+" from "+owner)
	}
	if err := reassignAdopted(ctx, db, d); err != nil {
		return err
	}
	return grantData(ctx, db, d)
}

// adopt hands d's objects to its owner role, once; whatever still signs in as
// the old owner keeps its access through the owner role.
func adopt(ctx context.Context, db *pgx.Conn, d Database, old string) error {
	if err := exec(ctx, db, "REASSIGN OWNED BY %s TO %s", old, d.Owner); err != nil {
		return err
	}
	return exec(ctx, db, "GRANT %s TO %s", d.Owner, old)
}

func ensureRoles(ctx context.Context, server *pgx.Conn, d Database, report *Report) error {
	for _, role := range []string{d.Owner, d.Data} {
		created, err := ensureRole(ctx, server, role)
		if err != nil {
			return err
		}
		if created {
			report.Created = append(report.Created, "role "+role)
		}
	}
	return nil
}

func ensureRole(ctx context.Context, server *pgx.Conn, role string) (bool, error) {
	var exists bool
	if err := server.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	return true, exec(ctx, server, "CREATE ROLE %s NOLOGIN", role)
}

// ensureDatabase creates d where it is absent and returns its owner where it was not.
func ensureDatabase(ctx context.Context, server *pgx.Conn, d Database) (string, error) {
	var owner string
	err := server.QueryRow(ctx, `SELECT r.rolname FROM pg_database d
		JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = $1`, d.Name).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", exec(ctx, server, "CREATE DATABASE %s OWNER %s", d.Name, d.Owner)
	}
	return owner, err
}

// adoptable refuses to adopt database from a role that is privileged, or that
// owns any other database.
func adoptable(ctx context.Context, server *pgx.Conn, role, database string) error {
	var ok bool
	err := server.QueryRow(ctx, `SELECT `+unprivileged+` FROM pg_roles r WHERE r.rolname = $1`, role, database).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("owned by %s, which is privileged or owns another database: %w", role, errUnadoptable)
	}
	return nil
}

// reassignAdopted hands the owner role what each role it adopted has created
// since: every member that cannot sign in, which is how an adopted owner is
// told from a login the Secret Store issued, and that adopting could have taken.
func reassignAdopted(ctx context.Context, db *pgx.Conn, d Database) error {
	owner := d.Owner
	rows, err := db.Query(ctx, `SELECT r.rolname FROM pg_auth_members a
		JOIN pg_roles r ON r.oid = a.member JOIN pg_roles o ON o.oid = a.roleid
		WHERE o.rolname = $1 AND NOT r.rolcanlogin AND `+unprivileged+` ORDER BY r.rolname`, owner, d.Name)
	if err != nil {
		return err
	}
	adopted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, role := range adopted {
		if err := exec(ctx, db, "REASSIGN OWNED BY %s TO %s", role, owner); err != nil {
			return err
		}
	}
	return nil
}

// grantData lets the data role read and write every table and sequence of the
// owner's schemas, now and as the owner creates more.
func grantData(ctx context.Context, db *pgx.Conn, d Database) error {
	if err := exec(ctx, db, "GRANT CONNECT ON DATABASE %s TO %s", d.Name, d.Data); err != nil {
		return err
	}
	rows, err := db.Query(ctx, `SELECT n.nspname FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner
		WHERE r.rolname IN ($1, 'pg_database_owner') AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
		ORDER BY n.nspname`, d.Owner)
	if err != nil {
		return err
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, schema := range schemas {
		for _, statement := range []string{
			"GRANT USAGE ON SCHEMA %s TO %s",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s",
			"GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA %s TO %s",
		} {
			if err := exec(ctx, db, statement, schema, d.Data); err != nil {
				return err
			}
		}
	}
	for _, statement := range []string{
		"ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s",
		"ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO %s",
	} {
		if err := exec(ctx, db, statement, d.Owner, d.Data); err != nil {
			return err
		}
	}
	return nil
}

// exec runs one statement whose every placeholder is an identifier, quoted.
func exec(ctx context.Context, conn *pgx.Conn, statement string, identifiers ...string) error {
	quoted := make([]any, len(identifiers))
	for i, name := range identifiers {
		quoted[i] = pgx.Identifier{name}.Sanitize()
	}
	_, err := conn.Exec(ctx, fmt.Sprintf(statement, quoted...))
	return err
}
