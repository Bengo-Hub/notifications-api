package main

import (
	"context"
	"database/sql"
	"log"
	"regexp"
	"time"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/database"
	"github.com/joho/godotenv"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationLockKey serializes migrations across pods: every replica runs this binary on start
// and two concurrent ent schema diffs can race on the same DDL (the pos-api 2026-07-26 outage).
// Arbitrary, stable and unique per service ("NTFM", notifications migrate).
const migrationLockKey int64 = 0x4E54_464D

// maskPassword masks the password in a database URL for logging.
func maskPassword(url string) string {
	re := regexp.MustCompile(`://([^:]+):([^@]+)@`)
	return re.ReplaceAllString(url, "://$1:****@")
}

func main() {
	_ = godotenv.Load()

	// Use LoadDatabaseOnly to avoid validation failures from missing
	// provider secrets or OAuth config during migration.
	dbCfg, err := config.LoadDatabaseOnly()
	if err != nil {
		log.Fatalf("load database config: %v", err)
	}
	if dbCfg.MigrateURL != "" {
		dbCfg.URL = dbCfg.MigrateURL
	} else {
		// A session advisory lock is only reliable on a direct connection, not via PgBouncer.
		log.Printf("WARNING: POSTGRES_MIGRATE_URL is not set; migrating through POSTGRES_URL")
	}

	log.Printf("connecting to database: %s", maskPassword(dbCfg.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The lock lives on its own single connection for the whole run; other pods block here
	// until this one has finished, then find nothing left to do.
	lockDB, err := sql.Open("pgx", dbCfg.URL)
	if err != nil {
		log.Fatalf("open lock connection: %v", err)
	}
	lockDB.SetMaxOpenConns(1)
	defer lockDB.Close()
	if _, err := lockDB.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		log.Fatalf("acquire migration lock: %v", err)
	}

	client, err := database.NewClient(ctx, *dbCfg)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	migrateErr := database.RunMigrations(ctx, client)
	client.Close()
	if _, err := lockDB.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
		log.Printf("release migration lock: %v", err)
	}
	if migrateErr != nil {
		log.Fatalf("migrate: %v", migrateErr)
	}
	log.Println("migrations completed")
}
