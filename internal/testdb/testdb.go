// Package testdb opens an isolated Postgres schema for tests that need the real database (query
// predicates, constraints, NULL handling). It uses the local PostgreSQL the team develops against
// (TEST_POSTGRES_URL, default postgres:postgres@localhost:5432/notifications) and skips the test
// when it is not reachable, so CI without a database still passes.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bengobox/notifications-api/internal/ent"
)

// DefaultURL is the local development database.
const DefaultURL = "postgres://postgres:postgres@localhost:5432/notifications?sslmode=disable"

// URL is the database tests use.
func URL() string {
	if v := os.Getenv("TEST_POSTGRES_URL"); v != "" {
		return v
	}
	return DefaultURL
}

// DB is one test's schema: an ent client with every table created, and the DSN that reaches the
// same schema (for pgx pools and raw SQL).
type DB struct {
	Client *ent.Client
	DSN    string
}

// Open creates a fresh schema named after the test, builds every table in it from the ent schema
// and drops it when the test ends. It skips the test when Postgres is unreachable.
func Open(t *testing.T) DB {
	t.Helper()
	base := URL()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Skipf("postgres: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("postgres unreachable (set TEST_POSTGRES_URL): %v", err)
	}
	schema := "ent_test_" + sanitize(t.Name())
	if _, err := admin.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE; CREATE SCHEMA %q", schema, schema)); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	dsn := base + sep + "search_path=" + schema
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_, _ = admin.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schema))
		_ = admin.Close()
	})
	return DB{Client: client, DSN: dsn}
}

// Pool opens a pgx pool on the test's schema, closed when the test ends.
func (d DB) Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), d.DSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func sanitize(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}
