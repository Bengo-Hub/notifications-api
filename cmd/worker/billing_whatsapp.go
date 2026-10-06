package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// sharedBooksBase is the domain the subscription_*_btn templates' buttons were approved against
// (templates.json): treasury-ui, which serves the public invoice page (/i/{token}) and the pay
// page (/pay?...).
const sharedBooksBase = "https://books.codevertexafrica.com"

// billingWhatsApp says which platform billing events also go to the tenant over WhatsApp, with
// which approved template.
type billingWhatsApp struct {
	template   string // Meta template name (URL button)
	textID     string // templates/whatsapp body, for the delivery log
	kind       string // "subscription" or "support"
	reminder   bool   // payment_due (days left) rather than invoice_ready (due date)
	dedupeByID bool   // repeats daily for the same aggregate
}

var billingWhatsAppEvents = map[string]billingWhatsApp{
	"invoice_generated":             {template: "subscription_invoice_ready_v1_btn", textID: "subscription/invoice_ready", kind: "subscription"},
	"support_fee_invoice_generated": {template: "subscription_invoice_ready_v1_btn", textID: "subscription/invoice_ready", kind: "support"},
	"grace_started":                 {template: "subscription_payment_due_v1_btn", textID: "subscription/payment_due", kind: "subscription", reminder: true},
	"grace_reminder":                {template: "subscription_payment_due_v1_btn", textID: "subscription/payment_due", kind: "subscription", reminder: true, dedupeByID: true},
	"support_fee_overdue":           {template: "subscription_payment_due_v1_btn", textID: "subscription/payment_due", kind: "support", reminder: true},
	"support_fee_grace_reminder":    {template: "subscription_payment_due_v1_btn", textID: "subscription/payment_due", kind: "support", reminder: true, dedupeByID: true},
}

// booksButtonSuffix is the button's dynamic part: the path (and query) of the invoice page link,
// else of the pay link, on the books domain the template allows. Empty when neither is a books
// link, so no button ever opens another site.
func booksButtonSuffix(links ...string) string {
	base, _ := url.Parse(sharedBooksBase)
	for _, l := range links {
		u, err := url.Parse(strings.TrimSpace(l))
		if err != nil || u.Host != base.Host {
			continue
		}
		p := strings.TrimPrefix(u.EscapedPath(), "/")
		if p == "" {
			continue
		}
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		return p
	}
	return ""
}

// billingPhone asks auth-api where the tenant's bills go by phone: the tenant administrator, else
// the main outlet, else the tenant's phone (GET /api/v1/s2s/{tenant}/billing-contact).
func billingPhone(ctx context.Context, cfg *config.Config, tenantID string) (string, string, error) {
	if cfg.Services.AuthAPI == "" || cfg.Security.APIKey == "" {
		return "", "", fmt.Errorf("auth url or internal key not configured")
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/s2s/%s/billing-contact", strings.TrimRight(cfg.Services.AuthAPI, "/"), url.PathEscape(tenantID)), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("X-API-Key", cfg.Security.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("auth billing-contact: status %d", resp.StatusCode)
	}
	var out struct {
		Phone  string `json:"phone"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return strings.TrimSpace(out.Phone), out.Source, nil
}

// billingWhatsAppParams fills the template's body variables in order.
func billingWhatsAppParams(spec billingWhatsApp, ti *tenantInfo, payload map[string]any) []string {
	name := waParam(ti.Name, "there")
	// Invoice events carry a number and a currency; grace reminders an already formatted string.
	amount := waParam(payload["amount"], "-")
	if n, isNum := payload["amount"].(float64); isNum {
		if cur, _ := payload["currency"].(string); cur != "" {
			amount = formatMoney(n, cur)
		}
	}
	invoice := waParam(payload["invoice_number"], "-")
	if spec.reminder {
		return []string{name, spec.kind, amount, invoice, waParam(payload["days_remaining"], "a few")}
	}
	return []string{name, spec.kind, invoice, amount, waParam(formatEventDate(payload["due_date"]), "on receipt")}
}

// sendBillingWhatsApp sends a platform billing event to the tenant over WhatsApp from the
// platform's number, with the invoice page as the button. Best effort: email already went; a
// missing phone, link or template just logs.
func sendBillingWhatsApp(ctx context.Context, nc *nats.Conn, cfg *config.Config, evt subscriptionEvent, tenantID string, ti *tenantInfo, logg *zap.Logger) {
	spec, ok := billingWhatsAppEvents[evt.EventType]
	if !ok || ti == nil {
		return
	}
	suffix := booksButtonSuffix(firstNonEmpty(evt.Payload["invoice_url"]), firstNonEmpty(evt.Payload["pay_url"], evt.Payload["pay_link"]))
	if suffix == "" {
		logg.Info("billing whatsapp skipped: no invoice link on the books domain", zap.String("type", evt.EventType), zap.String("tenant_id", tenantID))
		return
	}
	phone, source, err := billingPhone(ctx, cfg, tenantID)
	if err != nil || phone == "" {
		logg.Info("billing whatsapp skipped: no billing phone", zap.String("tenant_id", tenantID), zap.Error(err))
		return
	}
	params := billingWhatsAppParams(spec, ti, evt.Payload)
	meta := map[string]any{
		"service_id":            "subscriptions",
		"template_name":         spec.template,
		"template_language":     "en_US",
		"template_params":       params,
		"template_button_param": suffix,
	}
	if code := dialCodeForCountry(ti.Country); code != "" {
		meta["default_dial_code"] = code
	}
	data := map[string]any{"name": params[0], "kind": spec.kind, "invoice_number": evt.Payload["invoice_number"],
		"amount": evt.Payload["amount"], "due_date": formatEventDate(evt.Payload["due_date"]), "days_remaining": evt.Payload["days_remaining"]}
	key := fmt.Sprintf("billing-wa-%s-%s", evt.EventType, evt.AggregateID)
	if spec.dedupeByID && evt.ID != "" {
		key = fmt.Sprintf("billing-wa-%s-%s", evt.EventType, evt.ID)
	}
	msg := messaging.Message{
		TenantID:       tenantID,
		Channel:        "whatsapp",
		TemplateID:     spec.textID,
		SenderScope:    messaging.SenderScopePlatform,
		Target:         messaging.TargetTenantAdmin,
		To:             []string{phone},
		Data:           data,
		Metadata:       meta,
		RequestID:      uuid.New().String(),
		IdempotencyKey: key,
		QueuedAt:       time.Now(),
	}
	if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
		logg.Warn("billing whatsapp: dispatch failed", zap.String("type", evt.EventType), zap.Error(err))
		return
	}
	logg.Info("billing whatsapp dispatched", zap.String("type", evt.EventType), zap.String("tenant_id", tenantID), zap.String("phone_source", source))
}
