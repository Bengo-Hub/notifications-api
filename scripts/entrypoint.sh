#!/bin/sh
# Entrypoint script for Notifications-API service
# Runs migrations and seed before starting the server

set -e

echo "=========================================="
echo "Notifications-API Service Startup"
echo "=========================================="

# Wait for database to be ready (with timeout)
echo "Waiting for database connection..."
MAX_RETRIES=60
RETRY_COUNT=0

until /usr/local/bin/notifications-migrate > /dev/null 2>&1 || [ $RETRY_COUNT -eq $MAX_RETRIES ]; do
  RETRY_COUNT=$((RETRY_COUNT+1))
  echo "Database not ready yet or migrations failing... (attempt $RETRY_COUNT/$MAX_RETRIES)"
  sleep 5
done

if [ $RETRY_COUNT -eq $MAX_RETRIES ]; then
  echo "Database connection timeout after $MAX_RETRIES attempts"
  echo "Proceeding to start server anyway"
else
  echo "Database connected and migrations completed (attempt $RETRY_COUNT)"
fi

echo ""
echo "=========================================="
echo "Running seed (idempotent)"
echo "=========================================="
/usr/local/bin/notifications-seed || echo "Seed completed with warnings (non-fatal)"

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
