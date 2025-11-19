package config

import (
	"fmt"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

const namespace = "NOTIFICATIONS"

type Config struct {
	App       AppConfig
	HTTP      HTTPConfig
	Postgres  PostgresConfig
	Redis     RedisConfig
	Events    EventsConfig
	Providers ProviderConfig
	Templates TemplateConfig
	Security  SecurityConfig
}

type AppConfig struct {
	Name    string `envconfig:"APP_NAME" default:"notifications-app"`
	Env     string `envconfig:"APP_ENV" default:"development"`
	Version string `envconfig:"APP_VERSION" default:"0.1.0"`
}

type HTTPConfig struct {
	Host         string        `envconfig:"HTTP_HOST" default:"0.0.0.0"`
	Port         int           `envconfig:"HTTP_PORT" default:"4002"`
	ReadTimeout  time.Duration `envconfig:"HTTP_READ_TIMEOUT" default:"15s"`
	WriteTimeout time.Duration `envconfig:"HTTP_WRITE_TIMEOUT" default:"15s"`
	IdleTimeout  time.Duration `envconfig:"HTTP_IDLE_TIMEOUT" default:"60s"`
	TLSCertFile  string        `envconfig:"TLS_CERT_FILE"`
	TLSKeyFile   string        `envconfig:"TLS_KEY_FILE"`
}

type PostgresConfig struct {
	URL             string        `envconfig:"POSTGRES_URL" default:"postgres://postgres:postgres@localhost:5432/notifications?sslmode=disable"`
	MaxOpenConns    int           `envconfig:"POSTGRES_MAX_OPEN_CONNS" default:"20"`
	MaxIdleConns    int           `envconfig:"POSTGRES_MAX_IDLE_CONNS" default:"10"`
	ConnMaxLifetime time.Duration `envconfig:"POSTGRES_CONN_MAX_LIFETIME" default:"30m"`
}

type RedisConfig struct {
	Addr        string        `envconfig:"REDIS_ADDR" default:"localhost:6381"`
	Password    string        `envconfig:"REDIS_PASSWORD"`
	DB          int           `envconfig:"REDIS_DB" default:"0"`
	DialTimeout time.Duration `envconfig:"REDIS_DIAL_TIMEOUT" default:"5s"`
}

type EventsConfig struct {
	Bus        string        `envconfig:"EVENT_BUS" default:"nats"`
	NATSURL    string        `envconfig:"NATS_URL" default:"nats://localhost:4222"`
	StreamName string        `envconfig:"NATS_STREAM" default:"notifications"`
	Subject    string        `envconfig:"NATS_SUBJECT" default:"notifications.events"`
	AckWait    time.Duration `envconfig:"NATS_ACK_WAIT" default:"30s"`
}

type ProviderConfig struct {
	SendGridAPIKey         string `envconfig:"SENDGRID_API_KEY"`
	MailgunDomain          string `envconfig:"MAILGUN_DOMAIN"`
	MailgunAPIKey          string `envconfig:"MAILGUN_API_KEY"`
	TwilioAccountSID       string `envconfig:"TWILIO_ACCOUNT_SID"`
	TwilioAuthToken        string `envconfig:"TWILIO_AUTH_TOKEN"`
	AfricasTalkingKey      string `envconfig:"AFRICAS_TALKING_KEY"`
	AfricasTalkingUsername string `envconfig:"AFRICAS_TALKING_USERNAME"`
	VonageAPIKey           string `envconfig:"VONAGE_API_KEY"`
	VonageAPISecret        string `envconfig:"VONAGE_API_SECRET"`
	PlivoAuthID            string `envconfig:"PLIVO_AUTH_ID"`
	PlivoAuthToken         string `envconfig:"PLIVO_AUTH_TOKEN"`
	SMTPHost               string `envconfig:"SMTP_HOST" default:"localhost"`
	SMTPPort               int    `envconfig:"SMTP_PORT" default:"1025"`
	SMTPUsername           string `envconfig:"SMTP_USERNAME"`
	SMTPPassword           string `envconfig:"SMTP_PASSWORD"`
	SMTPFrom               string `envconfig:"SMTP_FROM" default:"no-reply@bengobox.com"`
	SMTPStartTLS           bool   `envconfig:"SMTP_STARTTLS" default:"false"`
	FCMServiceAccount      string `envconfig:"FCM_SERVICE_ACCOUNT"`
	APNSCert               string `envconfig:"APNS_CERT"`
	APNSKey                string `envconfig:"APNS_KEY"`
	DefaultEmailSender     string `envconfig:"DEFAULT_EMAIL_SENDER" default:"Urban Cafe <hello@bengobox.com>"`
	DefaultSMSSender       string `envconfig:"DEFAULT_SMS_SENDER" default:"BengoBox"`
	DefaultPushTopic       string `envconfig:"DEFAULT_PUSH_TOPIC" default:"general"`
}

type TemplateConfig struct {
	Directory string        `envconfig:"TEMPLATE_DIRECTORY" default:"./templates"`
	CacheTTL  time.Duration `envconfig:"TEMPLATE_CACHE_TTL" default:"5m"`
}

type SecurityConfig struct {
	// Optional shared API key for protecting /v1 endpoints. If empty, endpoints are open.
	APIKey string `envconfig:"API_KEY"`
	// Auth Service SSO (JWT) integration
	RequireJWT       bool   `envconfig:"REQUIRE_JWT" default:"true"`
	JWKSURL          string `envconfig:"JWKS_URL" default:"https://auth.codevertex.local:4101/api/v1/.well-known/jwks.json"`
	Issuer           string `envconfig:"JWT_ISSUER" default:"https://auth.codevertex.local:4101"`
	Audience         string `envconfig:"JWT_AUDIENCE" default:"notifications"`
	AuthServiceURL   string `envconfig:"AUTH_SERVICE_URL" default:"https://auth.codevertex.local:4101"`
	EnableAPIKeyAuth bool   `envconfig:"ENABLE_API_KEY_AUTH" default:"true"`
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	var cfg Config
	if err := envconfig.Process(namespace, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return &cfg, nil
}
