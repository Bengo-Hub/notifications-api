// Command export-wallets moves the SMS balances and active WhatsApp subscriptions held in this
// service's retired wallet tables into subscriptions-api, which now owns them. It only reads from
// this database and the import call is additive and idempotent, so it can be run repeatedly and
// the old tables stay untouched until someone has checked the result and drops them in a later
// release. It runs as a dry run unless -apply is passed.
//
//	export-wallets            # report what would move
//	export-wallets -apply     # move it
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/database"
	"github.com/bengobox/notifications-api/internal/ent/tenantcredit"
	"github.com/bengobox/notifications-api/internal/ent/tenantwhatsappsubscription"
)

// tierBySlug maps the retired plan slugs onto the tiers subscriptions-api sells.
var tierBySlug = map[string]string{"basic": "tier_1", "standard": "tier_2", "pro": "pro"}

type legacyWallet struct {
	TenantID        uuid.UUID  `json:"tenant_id"`
	SMSBalance      int64      `json:"sms_balance"`
	WhatsAppTier    string     `json:"whatsapp_tier,omitempty"`
	WhatsAppExpires *time.Time `json:"whatsapp_expires_at,omitempty"`
	WhatsAppUsed    int64      `json:"whatsapp_used,omitempty"`
}

func main() {
	apply := flag.Bool("apply", false, "write the balances into subscriptions-api (default is a dry run)")
	flag.Parse()
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx := context.Background()
	client, err := database.NewClient(ctx, cfg.Postgres)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer client.Close()

	wallets := map[uuid.UUID]*legacyWallet{}
	get := func(id uuid.UUID) *legacyWallet {
		if w, ok := wallets[id]; ok {
			return w
		}
		w := &legacyWallet{TenantID: id}
		wallets[id] = w
		return w
	}

	credits, err := client.TenantCredit.Query().Where(tenantcredit.TypeEQ(tenantcredit.TypeSMS), tenantcredit.BalanceGT(0)).All(ctx)
	if err != nil {
		log.Fatalf("read sms balances: %v", err)
	}
	perSMS := 1.0
	if pb, perr := client.PlatformBilling.Query().First(ctx); perr == nil && pb.CostPerSms > 0 {
		perSMS = pb.CostPerSms
	}
	for _, c := range credits {
		rate := perSMS
		if c.Rate > 0 {
			rate = c.Rate
		}
		get(c.TenantID).SMSBalance = int64(math.Floor(c.Balance / rate))
	}

	subs, err := client.TenantWhatsAppSubscription.Query().
		Where(tenantwhatsappsubscription.StatusEQ(tenantwhatsappsubscription.StatusActive), tenantwhatsappsubscription.ExpiresAtGT(time.Now())).
		WithPlan().
		All(ctx)
	if err != nil {
		log.Fatalf("read whatsapp subscriptions: %v", err)
	}
	for _, sub := range subs {
		if sub.Edges.Plan == nil {
			continue
		}
		tier, ok := tierBySlug[sub.Edges.Plan.Slug]
		if !ok {
			log.Printf("skip %s: unknown plan slug %q", sub.TenantID, sub.Edges.Plan.Slug)
			continue
		}
		exp := sub.ExpiresAt
		w := get(sub.TenantID)
		w.WhatsAppTier, w.WhatsAppExpires, w.WhatsAppUsed = tier, &exp, int64(sub.MessagesUsed)
	}

	items := make([]legacyWallet, 0, len(wallets))
	for _, w := range wallets {
		items = append(items, *w)
	}
	log.Printf("%d tenants hold balances (%d with SMS units, %d with an active WhatsApp plan)", len(items), len(credits), len(subs))

	base := strings.TrimRight(cfg.Services.SubscriptionsAPI, "/")
	for start := 0; start < len(items); start += 200 {
		end := start + 200
		if end > len(items) {
			end = len(items)
		}
		body, _ := json.Marshal(map[string]any{"dry_run": !*apply, "items": items[start:end]})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/s2s/credits/import", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", cfg.Security.APIKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Fatalf("import call: %v", err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Fatalf("import call: status %d: %s", resp.StatusCode, out)
		}
		var parsed struct {
			Results []struct {
				TenantID string   `json:"tenant_id"`
				Applied  []string `json:"applied"`
				Skipped  string   `json:"skipped"`
			} `json:"results"`
		}
		_ = json.Unmarshal(out, &parsed)
		for _, r := range parsed.Results {
			switch {
			case r.Skipped != "":
				fmt.Printf("%s  skipped: %s\n", r.TenantID, r.Skipped)
			default:
				fmt.Printf("%s  %s\n", r.TenantID, strings.Join(r.Applied, "; "))
			}
		}
	}
	if !*apply {
		fmt.Println("dry run only: nothing was written. Run again with -apply to move the balances.")
	}
}
