package providers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/encryption"
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

	// Test seams for the WhatsApp credential resolution; nil means the database.
	tenantOnlyWhatsApp func(ctx context.Context, tenantID string) (pcfg.Settings, error)
	platformMeta       func(ctx context.Context) (pcfg.Settings, error)
}

// TokenSourceKey is the tenant whatsapp/meta_cloud row that marks a managed link: a number a
// platform owner created in a WhatsApp account inside the Codevertex verified portfolio and linked
// to the tenant (POST /billing/whatsapp/managed-link). Its value TokenSourcePlatform means the
// platform system user's token, assigned to that account, is used with the tenant's own number
// and account IDs. It is not a secret.
const (
	TokenSourceKey      = "token_source"
	TokenSourcePlatform = "platform"
)

// loadTenantOnlyWhatsApp reads the tenant's own whatsapp/meta_cloud rows (no platform fallback).
func (m *Manager) loadTenantOnlyWhatsApp(ctx context.Context, tenantID string) (pcfg.Settings, error) {
	if m.tenantOnlyWhatsApp != nil {
		return m.tenantOnlyWhatsApp(ctx, tenantID)
	}
	return pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, tenantID, m.env, "whatsapp", "meta_cloud", m.decryptionKey)
}

// tenantWhatsAppSettings is the tenant's own whatsapp/meta_cloud settings with the access token
// resolved. Only a tenant row marked token_source=platform gets the platform token; without that
// marker the tenant's own token (or none) is kept, so a tenant number can never send with another
// account's token by accident.
func (m *Manager) tenantWhatsAppSettings(ctx context.Context, tenantID string) (pcfg.Settings, error) {
	s, err := m.loadTenantOnlyWhatsApp(ctx, tenantID)
	if err != nil {
		return s, err
	}
	if strings.TrimSpace(s[TokenSourceKey]) != TokenSourcePlatform {
		return s, nil
	}
	var platform pcfg.Settings
	if m.platformMeta != nil {
		platform, _ = m.platformMeta(ctx)
	} else {
		platform, _ = m.LoadPlatformMetaCredentials(ctx)
	}
	return applyPlatformToken(s, platform), nil
}

// applyPlatformToken returns a copy of a managed-link tenant's settings using the platform's
// access token (never a stale tenant token left behind) and, when the tenant has none, the
// platform's Graph API version. The tenant's own number and account IDs stay.
func applyPlatformToken(own, platform pcfg.Settings) pcfg.Settings {
	out := pcfg.Settings{}
	for k, v := range own {
		out[k] = v
	}
	out["access_token"] = strings.TrimSpace(platform["access_token"])
	if strings.TrimSpace(out["api_version"]) == "" {
		out["api_version"] = platform["api_version"]
	}
	return out
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
//
// The credentials live on the platform tenant's own whatsapp/meta_cloud row, the one its real
// sends and template management use (LoadWhatsAppTemplateCredentials). The literal "platform"
// tier is only the fallback: reading it alone found no access_token, so every Embedded Signup
// failed with "platform WhatsApp credentials not configured" (2026-11-12).
//
// The app ID and app secret (code exchange, webhook signatures) are saved from Platform >
// Providers, which writes the "platform" tier, so both tiers are merged: the platform tenant's
// non-empty values win over the tier's.
func (m *Manager) LoadPlatformMetaCredentials(ctx context.Context) (pcfg.Settings, error) {
	tier, tierErr := pcfg.LoadTenantProviderSettings(ctx, m.dbCfg, "platform", m.env, "whatsapp", "meta_cloud", m.decryptionKey)
	var own pcfg.Settings
	if m.PlatformID != "" {
		if s, err := pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, m.PlatformID, m.env, "whatsapp", "meta_cloud", m.decryptionKey); err == nil {
			own = s
		}
	}
	merged := mergeMetaCredentials(tier, own)
	if len(merged) == 0 && tierErr != nil {
		return nil, tierErr
	}
	return merged, nil
}

// mergeMetaCredentials overlays own's non-empty values on tier.
func mergeMetaCredentials(tier, own pcfg.Settings) pcfg.Settings {
	out := pcfg.Settings{}
	for k, v := range tier {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range own {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// EncryptSecret encrypts a provider secret with the same key the settings loaders decrypt with.
// It refuses rather than store a secret in plain text when no valid key is configured.
func (m *Manager) EncryptSecret(v string) (string, error) {
	if len(m.decryptionKey) != 32 {
		return "", errors.New("provider secret encryption key is not configured")
	}
	return encryption.Encrypt(v, m.decryptionKey)
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

// LoadTenantWhatsAppTemplateCredentials resolves the WABA ID and access token of a tenant's own
// WhatsApp Business Account (the rows Embedded Signup saves), for managing that WABA's message
// templates. Meta approves templates per WABA, so a tenant with its own number needs our templates
// on its own WABA before its template sends can work. Only the tenant's own whatsapp/meta_cloud
// row counts: there is deliberately no fallback to the platform's WABA, which would sync the
// platform's account while reporting it as the tenant's.
func (m *Manager) LoadTenantWhatsAppTemplateCredentials(ctx context.Context, tenantID string) (wabaID, accessToken string, err error) {
	if strings.TrimSpace(tenantID) == "" {
		return "", "", errors.New("tenant id is required")
	}
	s, loadErr := m.tenantWhatsAppSettings(ctx, tenantID)
	return tenantTemplateCredentials(s, loadErr)
}

// LoadTenantWhatsAppProfileCredentials resolves a tenant's own Cloud API number, the business
// token Embedded Signup saved for it and its Graph API version setting, for reading and editing
// that number's business profile. Like the template credentials, only the tenant's own
// whatsapp/meta_cloud row counts: never the platform's number.
func (m *Manager) LoadTenantWhatsAppProfileCredentials(ctx context.Context, tenantID string) (phoneNumberID, accessToken, apiVersion string, err error) {
	if strings.TrimSpace(tenantID) == "" {
		return "", "", "", errors.New("tenant id is required")
	}
	s, loadErr := m.tenantWhatsAppSettings(ctx, tenantID)
	return tenantProfileCredentials(s, loadErr)
}

// ErrNoTenantPhoneNumber: the tenant has not connected a WhatsApp number of its own.
var ErrNoTenantPhoneNumber = errors.New("no WhatsApp number is connected for this business: connect one under Settings > Providers > WhatsApp first")

// tenantProfileCredentials picks the number, token and version out of a tenant's own settings.
func tenantProfileCredentials(s pcfg.Settings, loadErr error) (string, string, string, error) {
	if loadErr != nil {
		return "", "", "", fmt.Errorf("load tenant WhatsApp settings: %w", loadErr)
	}
	id := strings.TrimSpace(s["phone_number_id"])
	token := strings.TrimSpace(s["access_token"])
	if id == "" {
		return "", "", "", ErrNoTenantPhoneNumber
	}
	if token == "" {
		return "", "", "", ErrNoTenantWABAToken
	}
	return id, token, strings.TrimSpace(s["api_version"]), nil
}

// ErrNoTenantWABA: the tenant has not connected a WhatsApp Business Account of its own.
var ErrNoTenantWABA = errors.New("no WhatsApp Business Account is connected for this tenant: connect a number under Settings > Providers > WhatsApp first")

// ErrNoTenantWABAToken: the tenant's WABA has no access token saved (a number entered by hand
// rather than through Embedded Signup), so its templates cannot be managed.
var ErrNoTenantWABAToken = errors.New("the connected WhatsApp Business Account has no access token saved: reconnect the number with Embedded Signup")

// tenantTemplateCredentials picks the WABA ID and token out of a tenant's own settings.
func tenantTemplateCredentials(s pcfg.Settings, loadErr error) (string, string, error) {
	if loadErr != nil {
		return "", "", fmt.Errorf("load tenant WhatsApp settings: %w", loadErr)
	}
	wabaID := strings.TrimSpace(s["waba_id"])
	token := strings.TrimSpace(s["access_token"])
	if wabaID == "" {
		return "", "", ErrNoTenantWABA
	}
	if token == "" {
		return "", "", ErrNoTenantWABAToken
	}
	return wabaID, token, nil
}

// ErrNoWhatsAppNumber: the tenant has no registered WhatsApp number of its own and may not use the
// platform's. Retrying cannot help, so the worker records the send as skipped.
var ErrNoWhatsAppNumber = errors.New("no WhatsApp number registered for this tenant")

// sharesPlatformNumber reports whether a tenant may send its own WhatsApp from the platform's number.
func (m *Manager) sharesPlatformNumber(tenantID string) bool {
	for _, id := range m.cfg.WhatsAppSharedNumberTenants {
		if strings.EqualFold(strings.TrimSpace(id), tenantID) {
			return true
		}
	}
	return false
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
			// A managed-link tenant (token_source=platform) resolves to its own number with the
			// platform system user's token; any other tenant keeps its own token.
			s, _ := m.tenantWhatsAppSettings(ctx, tenantID)
			accessToken := s["access_token"]
			phoneNumberID := s["phone_number_id"]
			apiVersion := s["api_version"]

			// A tenant's own messages go out only from its own registered number (owner,
			// 9 November 2026): a client's customers must never get the client's messages from
			// Codevertex's number. Platform-scope sends already resolve to the platform tenant
			// (worker providerTenantID). Only the allowlisted tenants (the demo tenant) may borrow
			// the platform number.
			if (accessToken == "" || phoneNumberID == "") && tenantID != m.PlatformID && m.sharesPlatformNumber(tenantID) {
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
	return nil, ErrNoWhatsAppNumber
}

// EmailReplyTo is the tenant's own Reply-To address for its emails (the `reply_to` setting of its
// SMTP or Brevo account), "" when none is set. Hadia's is info@hadiafrica.com while it sends from
// no-reply@ (owner, 10 November 2026). Only the tenant's own settings count, never the platform's.
func (m *Manager) EmailReplyTo(ctx context.Context, tenantID string) string {
	if tenantID == "" || tenantID == m.PlatformID {
		return ""
	}
	for _, name := range []string{"smtp", "brevo"} {
		s, err := pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, tenantID, m.env, "email", name, m.decryptionKey)
		if err == nil {
			if v := strings.TrimSpace(s["reply_to"]); strings.Contains(v, "@") {
				return v
			}
		}
	}
	return ""
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
		// A free plain text, never a paid template (see whatsAppTestSend).
		if to != "" {
			return nil, whatsAppTestSend(ctx, prov, to)
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
