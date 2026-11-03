package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/Bengo-Hub/shared-events/balancealert"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// One consumer for every low-balance alert on the platform (shared-events balancealert):
//
//   - tenant scope, from subscriptions-api: AI credits, eTIMS API tokens, SMS, WhatsApp and any
//     credit service it adds later. The tenant's admin gets an email and a WhatsApp message with
//     a Buy credits button to the subscriptions billing page.
//   - platform scope, from marketflow-ai: the platform owner's provider balances (fal,
//     Higgsfield, ElevenLabs and later providers). The platform owner gets the same two
//     channels with a Top up button to MarketFlow's providers page.
//
// The event carries the label, the numbers and the link, so a new credit service or provider
// needs no change here.

const balanceLowConsumer = "notifications-billing-balance-low"

// marketflowAppBase is the domain the provider_balance_low_v1_btn button was approved against.
const marketflowAppBase = "https://marketflow.codevertexafrica.com"

// platformOwnerSlug is the platform owner's tenant slug.
const platformOwnerSlug = "codevertex"

type balanceLowEvent struct {
	ID       string             `json:"id"`
	TenantID string             `json:"tenant_id"`
	Payload  balancealert.Alert `json:"payload"`
}

// balanceLowText is the plain wording of the alert, shared by both templates.
func balanceLowText(a balancealert.Alert) (remaining, threshold, headline string) {
	remaining = formatUnits(a.Remaining, a.Unit)
	threshold = formatUnits(a.Threshold, a.Unit)
	label := a.ResourceLabel
	if label == "" {
		label = a.Resource
	}
	if a.Level == balancealert.LevelEmpty {
		headline = label + " has run out"
	} else {
		headline = label + " is running low"
	}
	if a.Estimate {
		remaining = "about " + remaining
	}
	return
}

// formatUnits prints an amount in its unit: USD with cents, counts as whole numbers.
func formatUnits(v float64, unit string) string {
	switch strings.ToLower(unit) {
	case "usd":
		return fmt.Sprintf("USD %.2f", v)
	case "":
		return formatCount(v)
	}
	return formatCount(v) + " " + unit
}

func formatCount(v float64) string {
	n := int64(math.Floor(v + 1e-9))
	s := fmt.Sprintf("%d", n)
	if n < 1000 && n > -1000 {
		return s
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// balanceLowNotice builds the email and WhatsApp notice for one alert. ti is the tenant (tenant
// scope) or the platform owner (platform scope). alertEmails (PLATFORM_ALERT_EMAILS) receive a
// platform alert by email in place of the owner's contact email; the WhatsApp message goes to
// the owner's billing phone either way.
func balanceLowNotice(evt balanceLowEvent, ti *tenantInfo, email string, phones []string, alertEmails ...string) partyNotice {
	a := evt.Payload
	remaining, threshold, headline := balanceLowText(a)
	label := waParam(a.ResourceLabel, a.Resource)
	name := waParam(ti.Name, "there")
	levelWord := "is low"
	if a.Level == balancealert.LevelEmpty {
		levelWord = "has run out"
	}
	phone := ""
	if len(phones) > 0 {
		phone = phones[0]
	}
	base := evt.ID
	if base == "" {
		base = strings.Join([]string{a.Scope, a.TenantID, a.Resource, a.Level, a.Period}, "-")
	}
	data := map[string]any{
		"name": name, "resource_label": label, "level": a.Level, "headline": headline,
		"remaining": remaining, "threshold": threshold, "period": a.Period, "estimate": a.Estimate,
	}
	n := partyNotice{
		ServiceID: "billing",
		TenantID:  ti.ID.String(),
		Scope:     messaging.SenderScopePlatform,
		Country:   ti.Country,
		IdemBase:  "balance-low-" + base,
		Data:      data,
	}
	party := noticeParty{Email: email, Phone: phone}
	if a.Scope == balancealert.ScopePlatform {
		providersPage := marketflowAppBase + "/platform/providers"
		topUp := a.BuyURL
		if topUp == "" {
			topUp = providersPage
		}
		data["action_link"] = topUp
		data["providers_link"] = providersPage
		n.TemplateID = "billing/provider_balance_low"
		n.Subject = headline
		party.Target = messaging.TargetPlatformAdmin
		party.WhatsApp = &partyWhatsApp{
			Name:   "provider_balance_low_v1_btn",
			Params: []string{label, levelWord, remaining},
			Button: "platform/providers",
		}
	} else {
		data["action_link"] = pricingAppBase + "/billing"
		n.TemplateID = "billing/balance_low"
		n.Subject = headline + ": buy more to keep it going"
		party.Target = messaging.TargetTenantAdmin
		party.WhatsApp = &partyWhatsApp{
			Name:   "credits_balance_low_v1_btn",
			Params: []string{name, label, levelWord, remaining},
			Button: "billing",
		}
	}
	n.Parties = []noticeParty{party}
	if a.Scope == balancealert.ScopePlatform {
		seen := map[string]bool{}
		var extra []noticeParty
		for i, addr := range alertEmails {
			addr = strings.TrimSpace(addr)
			if !sendableEmail(addr) || seen[strings.ToLower(addr)] {
				continue
			}
			seen[strings.ToLower(addr)] = true
			extra = append(extra, noticeParty{Key: fmt.Sprintf("alert%d", i), Target: messaging.TargetPlatformAdmin,
				Email: addr, Channels: []string{"email"}})
		}
		if len(extra) > 0 {
			n.Parties[0].Email = "" // the alert list replaces the owner's contact email
			n.Parties[0].Channels = []string{"whatsapp"}
			n.Parties = append(n.Parties, extra...)
		}
	}
	return n
}

// handleBalanceLow resolves who to tell and sends the notice. A recipient with no contact is
// logged and the event acknowledged: there is nobody to tell.
func handleBalanceLow(ctx context.Context, nc *nats.Conn, cfg *config.Config, tr *tenantResolver, evt balanceLowEvent, logg *zap.Logger) error {
	a := evt.Payload
	var (
		ti  *tenantInfo
		err error
	)
	switch a.Scope {
	case balancealert.ScopeTenant:
		tid := a.TenantID
		if tid == "" {
			tid = evt.TenantID
		}
		if tid == "" || tid == uuid.Nil.String() {
			logg.Warn("balance alert: tenant scope without a tenant id")
			return nil
		}
		ti, err = tr.resolve(ctx, tid)
	case balancealert.ScopePlatform:
		ti, err = tr.resolveBySlug(ctx, platformOwnerSlug)
	default:
		logg.Warn("balance alert: unknown scope", zap.String("scope", a.Scope))
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve recipient: %w", err)
	}
	email := ti.ContactEmail
	phones, _, perr := billingPhones(ctx, cfg, ti.ID.String())
	if perr != nil {
		logg.Info("balance alert: no billing phone", zap.String("tenant_id", ti.ID.String()), zap.Error(perr))
	}
	var alertEmails []string
	if a.Scope == balancealert.ScopePlatform {
		alertEmails = strings.Split(cfg.Events.PlatformAlertEmails, ",")
	}
	n := balanceLowNotice(evt, ti, email, phones, alertEmails...)
	reachable := false
	for _, p := range n.Parties {
		if len(partyContacts(p)) > 0 {
			reachable = true
		}
	}
	if !reachable {
		logg.Warn("balance alert: nobody to tell", zap.String("tenant_id", ti.ID.String()), zap.String("resource", a.Resource))
		return nil
	}
	if err := sendToParties(ctx, nc, cfg, n); err != nil {
		return err
	}
	logg.Info("balance alert dispatched", zap.String("scope", a.Scope), zap.String("resource", a.Resource),
		zap.String("level", a.Level), zap.String("tenant_id", ti.ID.String()))
	return nil
}

// startBalanceLowConsumer subscribes to billing.balance.low on the subscription stream (which
// binds billing.>), with one durable deliver group across replicas and an idempotency store, so
// each alert is sent once.
func startBalanceLowConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping balance alert consumer: NATS not available")
		return
	}
	logg = logg.Named("balance-low")
	var store *eventslib.IdempotencyStore
	if db, err := sql.Open("pgx", cfg.Postgres.URL); err == nil {
		store = eventslib.NewIdempotencyStore(db)
		ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := store.EnsureSchema(ectx); err != nil {
			// Sends are still deduplicated by their idempotency keys.
			logg.Warn("processed_events unavailable; relying on message idempotency keys", zap.Error(err))
			store = nil
		}
		cancel()
	}

	handler := func(m *nats.Msg) {
		var evt balanceLowEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("balance alert: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}
		eventID, idErr := eventslib.EventIDFromMsg(m)
		if store != nil && idErr == nil {
			if done, err := store.AlreadyProcessed(ctx, eventID, balanceLowConsumer); err == nil && done {
				_ = m.Ack()
				return
			}
		}
		if err := handleBalanceLow(ctx, nc, cfg, tr, evt, logg); err != nil {
			logg.Error("balance alert failed", zap.Error(err))
			_ = m.NakWithDelay(30 * time.Second)
			return
		}
		if store != nil && idErr == nil {
			if err := store.MarkProcessed(ctx, eventID, balanceLowConsumer); err != nil {
				logg.Warn("balance alert: mark processed failed", zap.Error(err))
			}
		}
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "subscription", balancealert.Subject, balanceLowConsumer, handler,
		nats.BindStream("subscription"),
		nats.Durable(balanceLowConsumer),
		nats.ManualAck(),
		nats.AckWait(60*time.Second),
		nats.MaxDeliver(5),
	)
}
