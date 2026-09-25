package providers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bengobox/notifications-api/internal/config"
	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
	"github.com/bengobox/notifications-api/internal/providers/email"
	"github.com/bengobox/notifications-api/internal/providers/push"
	"github.com/bengobox/notifications-api/internal/providers/sms"
	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// Manager resolves providers per-tenant.
type Manager struct {
	cfg           config.ProviderConfig
	db            *pgxpool.Pool
	dbCfg         config.PostgresConfig
	decryptionKey []byte
	env           string
	PlatformID    string

	// Platform Web Push identity cache (see webpush_keys.go).
	vapidMu    sync.Mutex
	vapidCache *push.WebPushConfig
	vapidAt    time.Time
}

// NewManager creates a provider manager. decryptionKey is optional (32 bytes) for decrypting provider secrets at rest.
func NewManager(db *pgxpool.Pool, dbCfg config.PostgresConfig, cfg config.ProviderConfig, decryptionKey []byte, env string, platformID string) *Manager {
	if env == "" {
		env = "production"
	}
	return &Manager{db: db, dbCfg: dbCfg, cfg: cfg, decryptionKey: decryptionKey, env: env, PlatformID: platformID}
}

// LoadPlatformMetaCredentials resolves the platform's own meta_cloud access_token/api_version —
// used for Graph API management calls made on a tenant's behalf (e.g. Embedded Signup completion:
// subscribing our webhook to a newly connected WABA, registering a newly connected phone number)
// where the platform's Tech Provider access, not any per-tenant credential, is what's needed.
func (m *Manager) LoadPlatformMetaCredentials(ctx context.Context) (pcfg.Settings, error) {
	return pcfg.LoadTenantProviderSettings(ctx, m.dbCfg, "platform", m.env, "whatsapp", "meta_cloud", m.decryptionKey)
}

// LoadWhatsAppTemplateCredentials resolves the WABA ID + access token used for template
// management (create/list message_templates), read from the platform tenant's own whatsapp/
// meta_cloud settings — the same tenant-only row GetWhatsAppProvider resolves for real sends (see
// LoadTenantOnlyProviderSettings), not the separate literal-"platform" fallback tier. Returns a
// clear error if either piece is missing rather than a confusing downstream Meta 400.
func (m *Manager) LoadWhatsAppTemplateCredentials(ctx context.Context) (wabaID, accessToken string, err error) {
	s, loadErr := pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, m.PlatformID, m.env, "whatsapp", "meta_cloud", m.decryptionKey)
	if loadErr != nil {
		return "", "", loadErr
	}
	wabaID = s["waba_id"]
	accessToken = s["access_token"]
	if wabaID == "" {
		return "", "", fmt.Errorf("no waba_id configured on the platform's whatsapp/meta_cloud settings")
	}
	if accessToken == "" {
		return "", "", fmt.Errorf("no access_token configured on the platform's whatsapp/meta_cloud settings")
	}
	return wabaID, accessToken, nil
}

func (m *Manager) GetWhatsAppProvider(ctx context.Context, tenantID string, preferred string) (WhatsAppProvider, error) {
	// meta_cloud (official Meta WhatsApp Cloud API) is the ONLY supported provider — no BSP
	// per-message markup, Meta-hosted reliability. apiwap was removed (never used in production,
	// platform decision to standardize on Meta as the sole Tech Provider integration).
	order := []string{"meta_cloud"}
	if preferred != "" {
		order = append([]string{strings.ToLower(preferred)}, order...)
	}
	for _, name := range dedup(order) {
		switch name {
		case "meta_cloud":
			s, _ := pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, tenantID, m.env, "whatsapp", "meta_cloud", m.decryptionKey)
			accessToken := s["access_token"]
			phoneNumberID := s["phone_number_id"]
			apiVersion := s["api_version"]

			// Fall back to the platform tenant's own WhatsApp number when this tenant hasn't
			// configured one of their own -- mirroring GetEmailProvider's tenant-then-platform
			// pattern (and the notifications-ui Settings page's own stated behavior: "without
			// one, sends use the platform's shared number"). This used to be a deliberate
			// tenant-only, no-fallback lookup; DB-confirmed against the live platform that the
			// real "codevertex" tenant (m.PlatformID) already carries a complete, active
			// meta_cloud credential set (access_token/phone_number_id/waba_id/api_version) that
			// every other tenant with no credentials of their own was simply never allowed to
			// reach, hard-failing "no WhatsApp number configured for this tenant" instead.
			if (accessToken == "" || phoneNumberID == "") && tenantID != m.PlatformID {
				if fb, fbErr := pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, m.PlatformID, m.env, "whatsapp", "meta_cloud", m.decryptionKey); fbErr == nil {
					if accessToken == "" {
						accessToken = fb["access_token"]
					}
					if phoneNumberID == "" {
						phoneNumberID = fb["phone_number_id"]
					}
					if apiVersion == "" {
						apiVersion = fb["api_version"]
					}
				}
			}

			if accessToken == "" || phoneNumberID == "" {
				continue // no WhatsApp number configured for this tenant, and no platform fallback available
			}

			return whatsapp.NewMetaCloudProvider(whatsapp.MetaCloudConfig{
				AccessToken:   accessToken,
				PhoneNumberID: phoneNumberID,
				APIVersion:    apiVersion,
			}), nil
		}
	}
	return nil, fmt.Errorf("no WhatsApp number configured for this tenant, and no platform fallback available")
}

func (m *Manager) GetEmailProvider(ctx context.Context, tenantID string, preferred string) (EmailProvider, error) {
	// Default to SMTP per user preference. SendGrid was removed (platform decision — never used
	// in production, standardizing on SMTP/Brevo).
	order := []string{"smtp", "brevo"}
	if preferred != "" {
		order = append([]string{strings.ToLower(preferred)}, order...)
	}
	for _, name := range dedup(order) {
		switch name {
		case "smtp":
			// The tenant's own SMTP account as a whole, else the platform's (DB, then env); never
			// the platform's credentials against a tenant's host (see provider_scope.go). A
			// localhost tenant host outside development counts as unconfigured.
			tenantOwn, platform, merged := m.scopedSettings(ctx, tenantID, "email", "smtp")
			acct := resolveSMTP(tenantOwn, platform, merged, smtpAccount{
				Host:     m.cfg.SMTPHost,
				Port:     strconv.Itoa(m.cfg.SMTPPort),
				Username: m.cfg.SMTPUsername,
				Password: m.cfg.SMTPPassword,
				From:     m.cfg.SMTPFrom,
				StartTLS: boolToStr(m.cfg.SMTPStartTLS),
			}, m.env == "development")

			host := acct.Host
			port := parseInt(acct.Port)
			user := acct.Username
			pass := acct.Password
			from := acct.From
			startTLS := parseBool(acct.StartTLS)
			ssl := parseBool(acct.SSL) || port == 465

			// If the resolved host is still localhost in production, skip SMTP
			// and try the next provider (brevo, etc.)
			if m.env != "development" && isLocalhost(host) {
				fmt.Printf("[DEBUG] SMTP host is localhost in %s env for tenant %s, trying next provider\n", m.env, tenantID)
				continue
			}

			return email.NewSMTPProvider(email.SMTPConfig{
				Host:     host,
				Port:     port,
				Username: user,
				Password: pass,
				From:     from,
				StartTLS: startTLS,
				SSL:      ssl,
			}), nil
		case "brevo":
			tenantOwn, platform, merged := m.scopedSettings(ctx, tenantID, "email", "brevo")
			s, _ := resolveKeyedAccount(tenantOwn, platform, merged, "api_key", nil,
				[]string{"sender_email", "sender_name", "from"}, map[string]string{"api_key": m.cfg.BrevoAPIKey})
			apiKey := s["api_key"]
			senderEmail := firstNonEmpty(s["sender_email"], s["from"], m.cfg.DefaultEmailSender)
			senderName := firstNonEmpty(s["sender_name"], "Notifications")
			if apiKey == "" {
				continue
			}
			return email.NewBrevoProvider(email.BrevoConfig{
				APIKey:      apiKey,
				SenderName:  senderName,
				SenderEmail: senderEmail,
			}), nil
		}
	}
	// fallback to smtp with defaults
	return email.NewSMTPProvider(email.SMTPConfig{
		Host:     m.cfg.SMTPHost,
		Port:     m.cfg.SMTPPort,
		Username: m.cfg.SMTPUsername,
		Password: m.cfg.SMTPPassword,
		From:     m.cfg.SMTPFrom,
		StartTLS: m.cfg.SMTPStartTLS,
	}), nil
}

func (m *Manager) GetSMSProvider(ctx context.Context, tenantID string, preferred string) (SMSProvider, error) {
	// Africa's Talking is the ONLY supported SMS provider (local carrier relationships, better
	// deliverability/cost for the Kenya/Africa market). Twilio/Vonage/Plivo were removed —
	// platform decision to standardize on a single SMS provider, never used in production.
	order := []string{"africastalking"}
	if preferred != "" {
		order = append([]string{strings.ToLower(preferred)}, order...)
	}
	for _, name := range dedup(order) {
		switch name {
		case "africastalking":
			// The username belongs to the API key's account, so they resolve together; the sender
			// ID may be the tenant's own on the platform account (registered there).
			tenantOwn, platform, merged := m.scopedSettings(ctx, tenantID, "sms", "africastalking")
			s, _ := resolveKeyedAccount(tenantOwn, platform, merged, "api_key", []string{"username"}, []string{"from"},
				map[string]string{"api_key": m.cfg.AfricasTalkingKey, "username": m.cfg.AfricasTalkingUsername})
			user := s["username"]
			key := s["api_key"]
			from := firstNonEmpty(s["from"], m.cfg.DefaultSMSSender)
			return &africasTalkingAdapter{username: user, apiKey: key, from: from}, nil
		}
	}
	return &africasTalkingAdapter{username: m.cfg.AfricasTalkingUsername, apiKey: m.cfg.AfricasTalkingKey, from: m.cfg.DefaultSMSSender}, nil
}

// TestConnection loads platform config for the given channel/provider, builds the provider, and sends a test message to the given recipient.
// TestConnection confirms tenantID's resolved provider config (tenant override, falling back to
// the platform-wide config per LoadTenantProviderSettings's hierarchy — the same resolution a real
// send for that tenant would use) actually works. Platform-admin callers pass m.PlatformID to test
// the platform's own shared config; tenant-scoped callers pass their own tenant ID so "Test"
// actually exercises what that tenant configured, not the platform's.
//
// Prefers a non-billable AccountInfoProvider query (account balance, sender identity, phone
// number quality rating) over sending a real message — cheaper and doesn't require a recipient.
// A recipient (to) means the caller wants proof of an actual send — used for a real
// message-sending test, e.g. Meta App Review's "record a video of your app sending a message"
// evidence requirement, which an account-info-only check can't satisfy. No recipient means a
// lightweight credential check only, for providers that support one (AccountInfoProvider).
// Email has no such check, so it always requires `to` and always sends. Returns provider-reported
// info (nil for a real test send) so the caller can show it to the admin.
func (m *Manager) TestConnection(ctx context.Context, tenantID, channel, providerName, to string) (map[string]interface{}, error) {
	switch channel {
	case "email":
		if to == "" {
			return nil, fmt.Errorf("to is required to test an email provider")
		}
		prov, err := m.GetEmailProvider(ctx, tenantID, providerName)
		if err != nil {
			return nil, err
		}
		return nil, prov.SendEmail(ctx, "", []string{to}, nil, nil, "", "Test connection", "<p>Test notification from Notifications API.</p>", "Test notification from Notifications API.", nil)
	case "sms":
		prov, err := m.GetSMSProvider(ctx, tenantID, providerName)
		if err != nil {
			return nil, err
		}
		if to != "" {
			return nil, prov.SendSMS(ctx, "", []string{to}, "Test SMS from Notifications API.")
		}
		if infoProv, ok := prov.(AccountInfoProvider); ok {
			return infoProv.AccountInfo(ctx)
		}
		return nil, fmt.Errorf("to is required to test this SMS provider (no account-info check available)")
	case "whatsapp":
		prov, err := m.GetWhatsAppProvider(ctx, tenantID, providerName)
		if err != nil {
			return nil, err
		}
		// A free-form text body only works inside an active 24h reply window, so the test uses
		// "hello_world" — the sample template Meta pre-approves by default on every WhatsApp
		// Business Account specifically for this kind of connectivity/send check.
		if to != "" {
			return nil, prov.SendWhatsApp(ctx, "", []string{to}, "", map[string]interface{}{
				"template_name":     "hello_world",
				"template_language": "en_US",
			})
		}
		if infoProv, ok := prov.(AccountInfoProvider); ok {
			return infoProv.AccountInfo(ctx)
		}
		return nil, fmt.Errorf("to is required to test this WhatsApp provider (no account-info check available)")
	case "push":
		// Same tenant-then-platform resolution a real push uses. `to` is a device token for a
		// real test push; without it only the service account is checked.
		prov, err := m.GetPushProvider(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		if to != "" {
			return nil, prov.SendPush(ctx, []string{to}, "Test notification", "Push is working.", nil)
		}
		if infoProv, ok := prov.(AccountInfoProvider); ok {
			info, ierr := infoProv.AccountInfo(ctx)
			if info != nil {
				info["source"] = m.ResolvePush(ctx, tenantID).Source
			}
			return info, ierr
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// Helpers

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func parseInt(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}

func parseBool(s string) bool {
	return strings.EqualFold(s, "true") || s == "1" || strings.EqualFold(s, "yes")
}

func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// isLocalhost returns true if the host is a loopback/localhost address
// that would only work in a local development environment.
func isLocalhost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]" || h == "0.0.0.0"
}

func dedup(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// africasTalkingAdapter bridges to our existing stub implementation.
type africasTalkingAdapter struct {
	username string
	apiKey   string
	from     string
}

func (a *africasTalkingAdapter) Name() string { return "africastalking" }

func (a *africasTalkingAdapter) SendSMS(ctx context.Context, from string, to []string, body string) error {
	if from == "" {
		from = a.from
	}
	provider := sms.NewAfricasTalking(sms.AfricasTalkingConfig{Username: a.username, APIKey: a.apiKey, From: from})
	return provider.SendSMS(ctx, from, to, body)
}

// GetBalance exposes the real Africa's Talking account balance. Callers type-assert for this
// optional method (not part of the generic SMSProvider interface) — see cmd/worker's
// platform-scope SMS dispatch.
func (a *africasTalkingAdapter) GetBalance(ctx context.Context) (float64, error) {
	provider := sms.NewAfricasTalking(sms.AfricasTalkingConfig{Username: a.username, APIKey: a.apiKey, From: a.from})
	return provider.GetBalance(ctx)
}

// AccountInfo implements providers.AccountInfoProvider (see TestConnection).
func (a *africasTalkingAdapter) AccountInfo(ctx context.Context) (map[string]interface{}, error) {
	provider := sms.NewAfricasTalking(sms.AfricasTalkingConfig{Username: a.username, APIKey: a.apiKey, From: a.from})
	return provider.AccountInfo(ctx)
}
