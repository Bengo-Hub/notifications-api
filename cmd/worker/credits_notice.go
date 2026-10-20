package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// pricingAppBase is the domain the credits notices' buttons were approved against
// (templates.json credits_*_btn): the subscriptions app, whose billing page holds balances,
// bundles and the auto top-up setting.
const pricingAppBase = "https://pricing.codevertexafrica.com"

// eventAutoTopupFailed is published by subscriptions-api when an automatic bundle purchase was
// declined or could not be made and the tenant's auto top-up was switched off.
const eventAutoTopupFailed = "credits_auto_topup_failed"

// autoTopupFailedNotice tells the tenant admin, by email and WhatsApp, that auto top-up was
// switched off and why. It goes from the platform's number, so the tenant's own plan and
// bundles do not apply. The WhatsApp message needs a phone and, like every _btn template, its
// link suffix (the billing page).
func autoTopupFailedNotice(evt subscriptionEvent, tenantID string, ti *tenantInfo, email string, phones []string) partyNotice {
	service := waParam(evt.Payload["service_label"], "SMS")
	units, _ := evt.Payload["units"].(float64)
	amount := "your bundle"
	if a, ok := evt.Payload["amount"].(float64); ok && a > 0 {
		amount = formatMoney(a, "KES")
	}
	reason := waParam(evt.Payload["reason"], "the payment could not be made")
	name := waParam(ti.Name, "there")
	billingLink := pricingAppBase + "/billing"

	phone := ""
	if len(phones) > 0 {
		phone = phones[0]
	}
	base := evt.ID
	if base == "" {
		base = evt.AggregateID + "-" + time.Now().UTC().Format("20060102")
	}
	return partyNotice{
		ServiceID:  "subscriptions",
		TenantID:   tenantID,
		TemplateID: "subscription/credits_auto_topup_failed",
		Subject:    "Auto top-up switched off",
		Scope:      messaging.SenderScopePlatform,
		Country:    ti.Country,
		IdemBase:   "credits-auto-topup-failed-" + base,
		Data: map[string]any{
			"name": name, "service_label": service, "amount": amount, "units": int64(units),
			"reason": reason, "action_link": billingLink,
		},
		Parties: []noticeParty{{
			Target: messaging.TargetTenantAdmin,
			Email:  email,
			Phone:  phone,
			WhatsApp: &partyWhatsApp{
				Name:   "credits_auto_topup_failed_v1_btn",
				Params: []string{name, service, amount, reason},
				Button: "billing",
			},
		}},
	}
}

// sendAutoTopupFailed resolves the tenant admin's email and billing phone and sends the notice.
// A tenant with neither contact is logged and acknowledged: there is nobody to tell.
func sendAutoTopupFailed(ctx context.Context, nc *nats.Conn, cfg *config.Config, evt subscriptionEvent, tenantID string, ti *tenantInfo, logg *zap.Logger) error {
	email := ti.ContactEmail
	if notif, ok := evt.Payload["notification"].(map[string]any); ok {
		if re, _ := notif["recipient_email"].(string); re != "" {
			email = re
		}
	}
	phones, _, err := billingPhones(ctx, cfg, tenantID)
	if err != nil {
		logg.Info("auto top-up notice: no billing phone", zap.String("tenant_id", tenantID), zap.Error(err))
	}
	n := autoTopupFailedNotice(evt, tenantID, ti, email, phones)
	if len(partyContacts(n.Parties[0])) == 0 {
		logg.Warn("auto top-up notice: tenant has no contact to tell", zap.String("tenant_id", tenantID))
		return nil
	}
	if err := sendToParties(ctx, nc, cfg, n); err != nil {
		return fmt.Errorf("auto top-up notice: %w", err)
	}
	logg.Info("auto top-up failure notice dispatched", zap.String("tenant_id", tenantID))
	return nil
}
