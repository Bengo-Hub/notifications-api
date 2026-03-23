package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/migrate"
	_ "github.com/jackc/pgx/v5/stdlib" // register pgx driver
)

// NewClient initialises an Ent client backed by PostgreSQL.
func NewClient(ctx context.Context, cfg config.PostgresConfig) (*ent.Client, error) {
	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("open postgres connection: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))
	return client, nil
}

// RunMigrations executes Ent schema migrations idempotently.
// Tries Atlas versioned migrations first; falls back to Ent auto-migrate if Dir is unavailable.
func RunMigrations(ctx context.Context, client *ent.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if migrate.Dir != nil {
		if err := client.Schema.Create(ctx, schema.WithDir(migrate.Dir)); err != nil {
			return fmt.Errorf("run versioned migrations: %w", err)
		}
		return nil
	}
	// Fallback: Ent auto-migrate (creates/alters tables to match schemas)
	if err := client.Schema.Create(ctx); err != nil {
		return fmt.Errorf("run auto migrations: %w", err)
	}
	return nil
}
