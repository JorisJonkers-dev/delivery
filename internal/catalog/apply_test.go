package catalog_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JorisJonkers-dev/delivery/internal/catalog"
)

// The server the estate runs, pinned by tag: the catalog is held to the SQL a real one accepts.
const postgresImage = "postgres:17-alpine"

// devPostgres starts a Postgres and returns how to sign in to one of its
// databases as a login. It skips under -short, which runs without Docker.
func devPostgres(t *testing.T) func(user, password, database string) (*pgx.Conn, error) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Docker; skipped with -short")
	}
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env:          map[string]string{"POSTGRES_PASSWORD": "admin"},
			WaitingFor:   wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start Postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return func(user, password, database string) (*pgx.Conn, error) {
		config, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s dbname=%s sslmode=disable", host, port.Port(), database))
		if err != nil {
			return nil, err
		}
		config.User, config.Password = user, password
		return pgx.ConnectConfig(context.Background(), config)
	}
}

func run(t *testing.T, conn *pgx.Conn, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := conn.Exec(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func one[T any](t *testing.T, conn *pgx.Conn, query string, args ...any) T {
	t.Helper()
	var value T
	if err := conn.QueryRow(t.Context(), query, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

func TestTheCatalogCreatesAdoptsAndGrantsAndIsSafeToRepeat(t *testing.T) {
	signIn := devPostgres(t)
	as := func(user, password, database string) *pgx.Conn {
		t.Helper()
		conn, err := signIn(user, password, database)
		if err != nil {
			t.Fatalf("sign in as %s to %s: %v", user, database, err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}
	admin := as("postgres", "admin", "postgres")
	// What the old path made: a NOLOGIN owner of grimoire_db, a table it owns, and a login
	// that is a member of it and acts as it, as Vault's old roles and cluster.sh's do.
	run(t, admin,
		"CREATE ROLE grimoire_user NOLOGIN",
		"CREATE DATABASE grimoire_db OWNER grimoire_user",
		"CREATE ROLE legacy LOGIN PASSWORD 'legacy' IN ROLE grimoire_user",
		"ALTER ROLE legacy SET role = 'grimoire_user'",
		"CREATE DATABASE owned_db OWNER postgres")
	old := as("postgres", "admin", "grimoire_db")
	run(t, old,
		"ALTER SCHEMA public OWNER TO grimoire_user",
		"CREATE TABLE public.sessions (id serial PRIMARY KEY, body text)",
		"ALTER TABLE public.sessions OWNER TO grimoire_user")

	c := catalog.Catalog{Databases: []catalog.Database{
		{Name: "grimoire_db", Owner: "grimoire-owner", Data: "grimoire-data"},
		{Name: "tribelt_db", Owner: "tribelt-owner", Data: "tribelt-data"},
	}}
	connect := func(_ context.Context, database string) (*pgx.Conn, error) {
		return signIn("postgres", "admin", database)
	}

	report, err := catalog.Apply(t.Context(), connect, c)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(report.Adopted) != "[grimoire_db from grimoire_user]" {
		t.Fatalf("adopted %v", report.Adopted)
	}
	if fmt.Sprint(report.Created) != "[role grimoire-owner role grimoire-data role tribelt-owner role tribelt-data database tribelt_db]" {
		t.Fatalf("created %v", report.Created)
	}
	for database, owner := range map[string]string{"grimoire_db": "grimoire-owner", "tribelt_db": "tribelt-owner"} {
		if got := one[string](t, admin, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1", database); got != owner {
			t.Errorf("%s is owned by %s", database, got)
		}
	}
	if got := one[string](t, old, "SELECT tableowner FROM pg_tables WHERE tablename = 'sessions'"); got != "grimoire-owner" {
		t.Errorf("sessions is owned by %s", got)
	}

	// The old path's login keeps its access, and what it creates now is reassigned on the next run.
	legacy := as("legacy", "legacy", "grimoire_db")
	run(t, legacy, "INSERT INTO public.sessions (body) VALUES ('old path')", "CREATE TABLE public.since (id int)")

	// A login of the data role reads and writes, and changes no schema.
	run(t, admin, "CREATE ROLE reader LOGIN PASSWORD 'reader' IN ROLE \"grimoire-data\"", "ALTER ROLE reader SET role = 'grimoire-data'")
	reader := as("reader", "reader", "grimoire_db")
	run(t, reader, "INSERT INTO public.sessions (body) VALUES ('estate path')")
	if got := one[int](t, reader, "SELECT count(*)::int FROM public.sessions"); got != 2 {
		t.Errorf("the data role reads %d rows", got)
	}
	if _, err := reader.Exec(t.Context(), "CREATE TABLE public.forbidden (id int)"); err == nil {
		t.Error("the data role created a table")
	}

	// Again: nothing new to create or adopt, and what the old login made since is the owner's.
	again, err := catalog.Apply(t.Context(), connect, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Created) != 0 || len(again.Adopted) != 0 {
		t.Fatalf("second run created %v and adopted %v", again.Created, again.Adopted)
	}
	if got := one[string](t, old, "SELECT tableowner FROM pg_tables WHERE tablename = 'since'"); got != "grimoire-owner" {
		t.Errorf("since is owned by %s", got)
	}
	if got := one[int](t, reader, "SELECT count(*)::int FROM public.since"); got != 0 {
		t.Errorf("the data role reads %d rows of since", got)
	}

	// A database a superuser owns is never adopted.
	_, err = catalog.Apply(t.Context(), connect, catalog.Catalog{Databases: []catalog.Database{
		{Name: "owned_db", Owner: "owned-owner", Data: "owned-data"},
	}})
	if err == nil {
		t.Fatal("a database a superuser owns was adopted")
	}
}
