package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/broadcasts"
)

// Payment provider outage alerts (owner, 9 November 2026). treasury-api publishes
// treasury.gateway.degraded when a provider starts refusing payments (maintenance or repeated
// errors) and treasury.gateway.recovered when it starts a payment again
// (payments/provider_health.go). Ops are told on every channel they watch: email to the platform
// alert list, the Slack webhook and WhatsApp numbers kept in auth-api's platform_alert_channels.
// Each affected tenant's admins get one plain email when it starts and one when it ends.
const (
	gatewayDegradedTemplate  = "platform/gateway_degraded"
	gatewayRecoveredTemplate = "platform/gateway_recovered"
	paymentsDelayedTemplate  = "finance/payments_delayed"
	paymentsRestoredTemplate = "finance/payments_restored"
	// opsWhatsAppTemplate is Meta's approved service notice ("{{1}}, {{2}}: {{3}}"), so the ops
	// alert needs no new template approval.
	opsWhatsAppTemplate = "broadcast_service_notice_v1"
	// tenantAdminsPerAlert caps the addresses one tenant's alert goes to.
	tenantAdminsPerAlert = 3
)

type gatewayEvent struct {
	TenantID string `json:"tenant_id"`
	Payload  struct {
		Provider        string   `json:"provider"`
		Upstream        string   `json:"upstream"`
		Reason          string   `json:"reason"`
		Message         string   `json:"message"`
		Since           string   `json:"since"`
		RecoveredAt     string   `json:"recovered_at"`
		Minutes         int      `json:"minutes"`
		TenantID        string   `json:"tenant_id"`
		OutageStarted   bool     `json:"outage_started"`
		FirstForTenant  bool     `json:"first_for_tenant"`
		AffectedTenants []string `json:"affected_tenants"`
	} `json:"payload"`
}

// providerLabel names the provider the way a tenant knows it.
func providerLabel(provider, upstream string) string {
	switch {
	case provider == "payhero" || provider == "mpesa" || upstream == "sasapay":
		return "M-Pesa"
	case provider != "":
		return strings.ToUpper(provider[:1]) + provider[1:]
	}
	return "The payment provider"
}

// readableTime turns an RFC 3339 instant into East Africa time for people.
func readableTime(v string) string {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return v
	}
	return t.In(time.FixedZone("EAT", 3*60*60)).Format("2 Jan 2006, 15:04 EAT")
}

func (s *Subscriber) handleGatewayDegraded(msg *nats.Msg) {
	var e gatewayEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn("gateway_degraded: unmarshal failed", zap.Error(err))
		_ = msg.Ack()
		return
	}
	p := e.Payload
	tid := firstNonEmpty(p.TenantID, e.TenantID)
	label := providerLabel(p.Provider, p.Upstream)
	data := map[string]any{
		"provider": p.Provider, "provider_label": label, "upstream": p.Upstream, "reason": p.Reason,
		"message": p.Message, "since": readableTime(p.Since),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var admins []broadcasts.Person
	if tid != "" {
		admins = s.tenantAdmins(ctx, tid)
	}
	if p.OutageStarted {
		data["tenant_name"] = tid
		if len(admins) > 0 && admins[0].BusinessName != "" {
			data["tenant_name"] = admins[0].BusinessName
		}
		text := fmt.Sprintf("%s payments are delayed since %s (%s). Payers are sent to the paybill; payments still settle late. Treasury will report recovery.",
			label, readableTime(p.Since), p.Reason)
		s.alertOps(ctx, tid, gatewayDegradedTemplate, label+" payments are delayed", data, text)
	}
	if p.FirstForTenant && tid != "" {
		s.alertTenant(tid, admins, paymentsDelayedTemplate, label+" payments are slower than usual right now", data)
	}
	s.log.Info("gateway_degraded alert dispatched", zap.String("provider", p.Provider), zap.String("tenant_id", tid),
		zap.Bool("ops", p.OutageStarted), zap.Bool("tenant", p.FirstForTenant))
	_ = msg.Ack()
}

func (s *Subscriber) handleGatewayRecovered(msg *nats.Msg) {
	var e gatewayEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn("gateway_recovered: unmarshal failed", zap.Error(err))
		_ = msg.Ack()
		return
	}
	p := e.Payload
	label := providerLabel(p.Provider, p.Upstream)
	data := map[string]any{
		"provider": p.Provider, "provider_label": label, "upstream": p.Upstream, "since": readableTime(p.Since),
		"recovered_at": readableTime(p.RecoveredAt), "minutes": p.Minutes, "affected_count": len(p.AffectedTenants),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	text := fmt.Sprintf("%s payments are working again since %s, after about %d minutes. %d tenant(s) were affected.",
		label, readableTime(p.RecoveredAt), p.Minutes, len(p.AffectedTenants))
	s.alertOps(ctx, firstNonEmpty(e.TenantID, ""), gatewayRecoveredTemplate, label+" payments are working again", data, text)
	for _, tid := range p.AffectedTenants {
		s.alertTenant(tid, s.tenantAdmins(ctx, tid), paymentsRestoredTemplate, label+" payments are back to normal", data)
	}
	s.log.Info("gateway_recovered alert dispatched", zap.String("provider", p.Provider), zap.Int("tenants", len(p.AffectedTenants)))
	_ = msg.Ack()
}

// alertOps sends an ops alert by email, Slack and WhatsApp. Each channel is best effort.
func (s *Subscriber) alertOps(ctx context.Context, tenantID, template, subject string, data map[string]any, text string) {
	if to := s.platformAlertRecipients(); len(to) > 0 {
		s.publishWithMetadata(tenantID, "email", template, messaging.TargetPlatformAdmin, to, data, map[string]any{"subject": subject})
	}
	settings := s.platformAlertSettings(ctx)
	if hook := settings["slack_webhook_url"]; hook != "" {
		s.postSlack(ctx, hook, ":warning: "+text)
	}
	if phones := splitList(settings["alert_whatsapp_to"]); len(phones) > 0 {
		s.publishWithMetadata(tenantID, "whatsapp", "", messaging.TargetPlatformAdmin, phones,
			map[string]any{"body": text},
			map[string]any{"template_name": opsWhatsAppTemplate, "template_params": []string{"Ops team", "Codevertex Treasury", text}})
	}
}

// alertTenant emails a tenant's admins. Platform context: the tenant's preferences never mute it.
func (s *Subscriber) alertTenant(tenantID string, admins []broadcasts.Person, template, subject string, base map[string]any) {
	for _, p := range admins {
		for _, e := range p.Emails {
			data := make(map[string]any, len(base)+1)
			for k, v := range base {
				data[k] = v
			}
			data["name"] = firstNonEmpty(e.FirstName, p.BusinessName, "there")
			s.publishWithMetadata(tenantID, "email", template, messaging.TargetTenantAdmin, []string{e.Value}, data, map[string]any{"subject": subject})
		}
	}
}

// tenantAdmins reads a tenant's owner and admin emails from auth-api's reach list.
func (s *Subscriber) tenantAdmins(ctx context.Context, tenantID string) []broadcasts.Person {
	if s.authAPI == "" || s.internalKey == "" {
		return nil
	}
	reach := &broadcasts.AuthReach{BaseURL: s.authAPI, APIKey: s.internalKey, HTTP: &http.Client{Timeout: 10 * time.Second}}
	people, _, err := reach.Page(ctx, map[string]any{"filters": map[string]any{"tenant_ids": []any{tenantID}, "include_demo": true}}, nil, "", 5)
	if err != nil {
		s.log.Warn("gateway alert: tenant admins not read", zap.String("tenant_id", tenantID), zap.Error(err))
		return nil
	}
	n := 0
	for i := range people {
		kept := people[i].Emails[:0]
		for _, e := range people[i].Emails {
			if n < tenantAdminsPerAlert && (e.Source == "owner" || e.Source == "admin" || e.Source == "tenant") {
				kept = append(kept, e)
				n++
			}
		}
		people[i].Emails = kept
	}
	return people
}

// platformAlertSettings reads the ops channels auth-api keeps (platform_alert_channels):
// slack_webhook_url, alert_email_to, alert_whatsapp_to. Empty on any failure.
func (s *Subscriber) platformAlertSettings(ctx context.Context) map[string]string {
	out := map[string]string{}
	if s.authAPI == "" || s.internalKey == "" {
		return out
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.authAPI+"/api/v1/s2s/platform-alert-settings", nil)
	if err != nil {
		return out
	}
	req.Header.Set("X-API-Key", s.internalKey)
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		s.log.Warn("gateway alert: platform alert settings not read", zap.Error(err))
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return out
}

// postSlack posts text to an incoming webhook.
func (s *Subscriber) postSlack(ctx context.Context, hook, text string) {
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		s.log.Warn("gateway alert: slack post failed", zap.Error(err))
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.log.Warn("gateway alert: slack refused", zap.Int("status", resp.StatusCode))
	}
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
