package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	_ "github.com/bengobox/notifications-api/internal/http/docs"

	"github.com/bengobox/notifications-api/internal/app"
)

// @title Notifications Service API
// @version 0.1.0
// @description HTTP API for the Codevertex notifications service.
// @BasePath /api/v1
// @schemes http https
// @host notificationsapi.codevertexitsolutions.com
// @securityDefinitions.apikey bearerAuth
// @in header
// @name Authorization
// @description JWT token from auth-service. Format: Bearer {token}
// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name X-API-Key
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx)
	if err != nil {
		log.Fatalf("failed to initialise app: %v", err)
	}
	defer a.Close()

	if err := a.Run(ctx); err != nil {
		log.Fatalf("runtime error: %v", err)
	}
}
