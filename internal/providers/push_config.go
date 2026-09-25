package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
	"github.com/bengobox/notifications-api/internal/providers/push"
)

// Push (FCM) is configured once here, in notifications-service, and reused by every app that
// wants web push (rider-app, notifications-ui, ...). Two halves belong to the same Firebase
// project and must always come from the same tier:
//   - server side: the service account that signs sends (secret, never leaves this service);
//   - browser side: the web app config and VAPID key a page needs to get a device token (public,
//     served by GET /api/v1/push/web-config so apps need no Firebase build settings of their own).
//
// Tiers, first complete one wins:
//  1. the tenant's own Firebase project (provider settings saved under the tenant), for a tenant
//     that wants its own sender identity;
//  2. the platform's shared project (settings saved under tenant "platform" by platform admins);
//  3. environment variables (PROVIDERS_FCM_SERVICE_ACCOUNT, PROVIDERS_FCM_WEB_*).
// Tiers are never mixed: a device token issued by one Firebase project cannot be sent to with
// another project's service account.

// WebPushConfig is the public Firebase client config a browser needs to register for push.
type WebPushConfig struct {
	APIKey            string `json:"api_key"`
	AuthDomain        string `json:"auth_domain,omitempty"`
	ProjectID         string `json:"project_id"`
	StorageBucket     string `json:"storage_bucket,omitempty"`
	MessagingSenderID string `json:"messaging_sender_id"`
	AppID             string `json:"app_id"`
	VAPIDKey          string `json:"vapid_key"`
}

// Complete reports whether a browser can register with this config.
func (w WebPushConfig) Complete() bool {
	return w.APIKey != "" && w.ProjectID != "" && w.MessagingSenderID != "" && w.AppID != "" && w.VAPIDKey != ""
}

// PushSettings is the resolved push configuration for one tenant.
type PushSettings struct {
	// Source is "tenant", "platform" or "env".
	Source         string
	ServiceAccount string
	ProjectID      string
	Web            WebPushConfig
}

// Ready reports whether sends can be made.
func (p PushSettings) Ready() bool { return p.ServiceAccount != "" && p.ProjectID != "" }

func pushSettingsFrom(source string, s map[string]string) PushSettings {
	sa := strings.TrimSpace(s["service_account"])
	project := strings.TrimSpace(s["project_id"])
	if project == "" && sa != "" {
		var key struct {
			ProjectID string `json:"project_id"`
		}
		if json.Unmarshal([]byte(sa), &key) == nil {
			project = key.ProjectID
		}
	}
	web := WebPushConfig{
		APIKey:            strings.TrimSpace(s["web_api_key"]),
		AuthDomain:        strings.TrimSpace(s["web_auth_domain"]),
		ProjectID:         project,
		StorageBucket:     strings.TrimSpace(s["web_storage_bucket"]),
		MessagingSenderID: strings.TrimSpace(s["web_messaging_sender_id"]),
		AppID:             strings.TrimSpace(s["web_app_id"]),
		VAPIDKey:          strings.TrimSpace(s["web_vapid_key"]),
	}
	if web.AuthDomain == "" && project != "" {
		web.AuthDomain = project + ".firebaseapp.com"
	}
	return PushSettings{Source: source, ServiceAccount: sa, ProjectID: project, Web: web}
}

// ResolvePush returns the push configuration a tenant uses (see the tier rules above).
func (m *Manager) ResolvePush(ctx context.Context, tenantID string) PushSettings {
	if tenantID != "" && tenantID != "platform" {
		if s, err := pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, tenantID, m.env, "push", "fcm", m.decryptionKey); err == nil {
			if t := pushSettingsFrom("tenant", s); t.Ready() {
				return t
			}
		}
	}
	if s, err := pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, "platform", m.env, "push", "fcm", m.decryptionKey); err == nil {
		if p := pushSettingsFrom("platform", s); p.Ready() {
			return p
		}
	}
	return pushSettingsFrom("env", map[string]string{
		"service_account":         m.cfg.FCMServiceAccount,
		"project_id":              m.cfg.FCMProjectID,
		"web_api_key":             m.cfg.FCMWebAPIKey,
		"web_auth_domain":         m.cfg.FCMWebAuthDomain,
		"web_storage_bucket":      m.cfg.FCMWebStorageBucket,
		"web_messaging_sender_id": m.cfg.FCMWebMessagingSenderID,
		"web_app_id":              m.cfg.FCMWebAppID,
		"web_vapid_key":           m.cfg.FCMWebVAPIDKey,
	})
}

// GetPushProvider returns the FCM provider for the tenant's resolved push configuration.
func (m *Manager) GetPushProvider(ctx context.Context, tenantID string) (PushProvider, error) {
	ps := m.ResolvePush(ctx, tenantID)
	if !ps.Ready() {
		return nil, fmt.Errorf("push: no Firebase service account configured for this tenant or the platform")
	}
	return push.NewFCM(push.FCMConfig{ProjectID: ps.ProjectID, ServiceAccount: ps.ServiceAccount}), nil
}
