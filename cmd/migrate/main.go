package main

import (
	"context"
	"log"
	"regexp"
	"time"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/database"
	"github.com/joho/godotenv"
)

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

	log.Printf("connecting to database: %s", maskPassword(dbCfg.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client, err := database.NewClient(ctx, *dbCfg)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer client.Close()

	if err := database.RunMigrations(ctx, client); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("migrations completed")
}
