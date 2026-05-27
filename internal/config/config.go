package config

import (
	"fmt"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

// Empty namespace so envconfig reads unprefixed keys (POSTGRES_URL, REDIS_ADDR, etc.).
const namespace = ""

// Config captures environment configuration for the Notifications-API service.
// Note: Empty envconfig tags on nested structs prevent field names from being added to the key prefix.
// Without empty tags, envconfig adds field names to prefix, e.g., Postgres.URL becomes POSTGRES_POSTGRES_URL.
// With empty tags, it becomes POSTGRES_URL as expected.
type Config struct {
	App       AppConfig      `envconfig:""`
	HTTP      HTTPConfig     `envconfig:""`
	Postgres  PostgresConfig `envconfig:""`
	Redis     RedisConfig    `envconfig:""`
	Events    EventsConfig   `envconfig:""`
	Providers ProviderConfig `envconfig:""`
	Templates TemplateConfig `envconfig:""`
	Security  SecurityConfig `envconfig:""`
	Services  ServicesConfig `envconfig:""`
}

type ServicesConfig struct {
	AuthAPI          string `envconfig:"SERVICES_AUTH_API_URL" default:"https://sso.codevertexitsolutions.com"`
	TreasuryAPI      string `envconfig:"TREASURY_API_URL" default:"https://booksapi.codevertexitsolutions.com"`
	SubscriptionsAPI string `envconfig:"SERVICES_SUBSCRIPTIONS_API_URL" default:"https://pricingapi.codevertexitsolutions.com"`
	SubscriptionsURL string `envconfig:"SERVICES_SUBSCRIPTIONS_UPGRADE_URL" default:"https://pricingapi.codevertexitsolutions.com/upgrade"`
}

type AppConfig struct {
	Name    string `envconfig:"APP_NAME" default:"notifications-api"`
	Env     string `envconfig:"APP_ENV" default:"development"`
	Version string `envconfig:"APP_VERSION" default:"0.1.0"`
}

type HTTPConfig struct {
	Host           string        `envconfig:"HTTP_HOST" default:"0.0.0.0"`
	Port           int           `envconfig:"HTTP_PORT" default:"4004"`
	ReadTimeout    time.Duration `envconfig:"HTTP_READ_TIMEOUT" default:"15s"`
	WriteTimeout   time.Duration `envconfig:"HTTP_WRITE_TIMEOUT" default:"15s"`
	IdleTimeout    time.Duration `envconfig:"HTTP_IDLE_TIMEOUT" default:"60s"`
	TLSCertFile    string        `envconfig:"HTTP_TLS_CERT_FILE"`
	TLSKeyFile     string        `envconfig:"HTTP_TLS_KEY_FILE"`
	AllowedOrigins []string      `envconfig:"HTTP_ALLOWED_ORIGINS" default:"https://notifications.codevertexitsolutions.com,https://ordersapp.codevertexitsolutions.com,https://accounts.codevertexitsolutions.com,https://theurbanloftcafe.com,http://localhost:3000,http://localhost:3001,http://127.0.0.1:3000"`
}

type PostgresConfig struct {
	URL             string        `envconfig:"POSTGRES_URL" default:"postgres://postgres:postgres@localhost:5432/notifications?sslmode=disable"`
	MigrateURL      string        `envconfig:"POSTGRES_MIGRATE_URL"` // direct PG for migrate/seed; bypasses PgBouncer transaction mode
	MaxOpenConns    int           `envconfig:"POSTGRES_MAX_OPEN_CONNS" default:"4"`
	MaxIdleConns    int           `envconfig:"POSTGRES_MAX_IDLE_CONNS" default:"2"`
	ConnMaxLifetime time.Duration `envconfig:"POSTGRES_CONN_MAX_LIFETIME" default:"5m"`
}

type RedisConfig struct {
	Addr        string        `envconfig:"REDIS_ADDR" default:"localhost:6381"`
	Password    string        `envconfig:"REDIS_PASSWORD"`
	DB          int           `envconfig:"REDIS_DB" default:"0"`
	DialTimeout time.Duration `envconfig:"REDIS_DIAL_TIMEOUT" default:"5s"`
}

type EventsConfig struct {
	Bus        string        `envconfig:"EVENTS_BUS" default:"nats"`
	NATSURL    string        `envconfig:"EVENTS_NATS_URL" default:"nats://localhost:4222"`
	StreamName string        `envconfig:"EVENTS_NATS_STREAM" default:"notifications"`
	Subject    string        `envconfig:"EVENTS_NATS_SUBJECT" default:"notifications.events"`
	AckWait    time.Duration `envconfig:"EVENTS_NATS_ACK_WAIT" default:"30s"`
}

type ProviderConfig struct {
	SendGridAPIKey         string `envconfig:"PROVIDERS_SENDGRID_API_KEY"`
	BrevoAPIKey            string `envconfig:"PROVIDERS_BREVO_API_KEY"`
	MailgunDomain          string `envconfig:"PROVIDERS_MAILGUN_DOMAIN"`
	MailgunAPIKey          string `envconfig:"PROVIDERS_MAILGUN_API_KEY"`
	TwilioAccountSID       string `envconfig:"PROVIDERS_TWILIO_ACCOUNT_SID"`
	TwilioAuthToken        string `envconfig:"PROVIDERS_TWILIO_AUTH_TOKEN"`
	AfricasTalkingKey      string `envconfig:"PROVIDERS_AFRICAS_TALKING_KEY"`
	AfricasTalkingUsername string `envconfig:"PROVIDERS_AFRICAS_TALKING_USERNAME"`
	VonageAPIKey           string `envconfig:"PROVIDERS_VONAGE_API_KEY"`
	VonageAPISecret        string `envconfig:"PROVIDERS_VONAGE_API_SECRET"`
	PlivoAuthID            string `envconfig:"PROVIDERS_PLIVO_AUTH_ID"`
	PlivoAuthToken         string `envconfig:"PROVIDERS_PLIVO_AUTH_TOKEN"`
	SMTPHost               string `envconfig:"PROVIDERS_SMTP_HOST"`
	SMTPPort               int    `envconfig:"PROVIDERS_SMTP_PORT" default:"587"`
	SMTPUsername           string `envconfig:"PROVIDERS_SMTP_USERNAME"`
	SMTPPassword           string `envconfig:"PROVIDERS_SMTP_PASSWORD"`
	SMTPFrom               string `envconfig:"PROVIDERS_SMTP_FROM" default:"no-reply@bengobox.com"`
	SMTPStartTLS           bool   `envconfig:"PROVIDERS_SMTP_STARTTLS" default:"false"`
	FCMServiceAccount      string `envconfig:"PROVIDERS_FCM_SERVICE_ACCOUNT"`
	APNSCert               string `envconfig:"PROVIDERS_APNS_CERT"`
	APNSKey                string `envconfig:"PROVIDERS_APNS_KEY"`
	DefaultEmailSender     string `envconfig:"PROVIDERS_DEFAULT_EMAIL_SENDER" default:"Urban Cafe <hello@bengobox.com>"`
	DefaultSMSSender       string `envconfig:"PROVIDERS_DEFAULT_SMS_SENDER" default:codevertex`
	DefaultPushTopic       string `envconfig:"PROVIDERS_DEFAULT_PUSH_TOPIC" default:"general"`
}

type TemplateConfig struct {
	Directory string        `envconfig:"TEMPLATES_DIRECTORY" default:"./templates"`
	CacheTTL  time.Duration `envconfig:"TEMPLATES_CACHE_TTL" default:"5m"`
}

type SecurityConfig struct {
	// Shared internal service key for S2S calls (same key across all services).
	APIKey string `envconfig:"INTERNAL_SERVICE_KEY"`
	// Auth Service SSO (JWT) integration
	RequireJWT bool   `envconfig:"SECURITY_REQUIRE_JWT" default:"true"`
	JWKSURL    string `envconfig:"SECURITY_JWKS_URL" default:"https://sso.codevertexitsolutions.com/api/v1/.well-known/jwks.json"`
	Issuer     string `envconfig:"SECURITY_JWT_ISSUER" default:"https://sso.codevertexitsolutions.com"`
	Audience   string `envconfig:"SECURITY_JWT_AUDIENCE" default:"codevertex"`
	// API key validation database URL (optional, enables API key authentication)
	APIKeyDBURL string `envconfig:"SECURITY_API_KEY_DB_URL"`
	// Enable API key authentication as fallback when JWT is not provided (default: true)
	EnableAPIKeyAuth bool `envconfig:"SECURITY_ENABLE_API_KEY_AUTH" default:"true"`
	// Encryption key for provider secrets at rest (32 bytes, base64). If empty, secrets are stored plain.
	EncryptionKey string `envconfig:"SECURITY_ENCRYPTION_KEY"`
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	var cfg Config
	if err := envconfig.Process(namespace, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// LoadDatabaseOnly loads only PostgresConfig, avoiding validation failures
// from missing provider secrets or OAuth config during migration/seed.
func LoadDatabaseOnly() (*PostgresConfig, error) {
	_ = godotenv.Load()

	var cfg PostgresConfig
	if err := envconfig.Process(namespace, &cfg); err != nil {
		return nil, fmt.Errorf("config(db): %w", err)
	}
	return &cfg, nil
}
