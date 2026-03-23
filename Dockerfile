# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git ca-certificates
# go.mod uses remote replace directive for auth-client, no local copy needed
# Build context is the service directory root
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=auto go mod download
COPY . .

# Build all binaries with service-prefixed names
RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/notifications-api ./cmd/api && \
    GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/notifications-worker ./cmd/worker && \
    GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/notifications-migrate ./cmd/migrate && \
    GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/notifications-seed ./cmd/seed

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata postgresql-client && addgroup -S app && adduser -S app -G app
WORKDIR /app
# Copy all binaries to well-known locations
COPY --from=builder /bin/notifications-api /usr/local/bin/notifications-api
COPY --from=builder /bin/notifications-worker /usr/local/bin/notifications-worker
COPY --from=builder /bin/notifications-migrate /usr/local/bin/notifications-migrate
COPY --from=builder /bin/notifications-seed /usr/local/bin/notifications-seed
# Entrypoint script: wait for DB, run migrations, seed, then start server
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
# Atlas migration files (so LocalDir can find them at runtime)
COPY --from=builder /src/internal/ent/migrate/migrations /app/internal/ent/migrate/migrations
# Notification templates (email, sms, push, whatsapp)
COPY --from=builder /src/templates /app/templates
RUN chown -R app:app /app/templates
# TLS certificates directory (optional, can be mounted as volume)
RUN mkdir -p ./config/certs
USER app
EXPOSE 4000
ENV PORT=4000
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
