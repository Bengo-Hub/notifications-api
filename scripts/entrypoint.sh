#!/bin/sh
# Entrypoint script for Notifications-API service
# Runs migrations and seed before starting the server

set -e

# Use direct PostgreSQL URL for migrate/seed to bypass PgBouncer transaction mode.
MIGRATE_URL="${POSTGRES_MIGRATE_URL:-$POSTGRES_URL}"

echo "=========================================="
echo "Notifications-API Service Startup"
echo "=========================================="

echo "Waiting for database and running migrations..."
MAX_RETRIES=60
RETRY_COUNT=0

until POSTGRES_URL="$MIGRATE_URL" /usr/local/bin/notifications-migrate > /dev/null 2>&1 || [ $RETRY_COUNT -eq $MAX_RETRIES ]; do
  RETRY_COUNT=$((RETRY_COUNT+1))
  echo "Database not ready yet... (attempt $RETRY_COUNT/$MAX_RETRIES)"
  sleep 5
done

if [ $RETRY_COUNT -eq $MAX_RETRIES ]; then
  echo "Database connection timeout after $MAX_RETRIES attempts"
  exit 1
fi

echo "Migrations applied successfully"

echo ""
echo "=========================================="
echo "Running seed (idempotent)"
echo "=========================================="
POSTGRES_URL="$MIGRATE_URL" /usr/local/bin/notifications-seed || echo "Seed completed with warnings (non-fatal)"

echo ""
echo "=========================================="
echo "Starting Notifications Worker (background)"
echo "=========================================="
/usr/local/bin/notifications-worker &
WORKER_PID=$!
echo "Worker started (PID: $WORKER_PID)"

echo ""
echo "=========================================="
echo "Starting Notifications-API server"
echo "=========================================="
echo ""

exec /usr/local/bin/notifications-api
