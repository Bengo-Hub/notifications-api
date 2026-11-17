package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// taxProposalsPendingEvent is treasury's notice that its weekly tax source watch found statutory
// rate or guidance changes for the platform owner to approve (payload: count, codes, review_path).
const taxProposalsPendingEvent = "tax.advisor_proposals_pending"

// treasuryAppBase is treasury-ui (Books), where the Tax advisor page lives.
const treasuryAppBase = "https://books.codevertexafrica.com"

// taxProposalsData builds the template data: how many proposals, which rate codes, and a link to
// the Updates tab of the platform owner's Tax advisor page.
func taxProposalsData(payload map[string]any) map[string]any {
	count := 0
	switch v := payload["count"].(type) {
	case float64:
		count = int(v)
	case int:
		count = v
	}
	var codes []string
	if raw, ok := payload["codes"].([]any); ok {
		for _, c := range raw {
			if s, ok := c.(string); ok && strings.TrimSpace(s) != "" {
				codes = append(codes, strings.TrimSpace(s))
			}
		}
	}
	if len(codes) > 10 {
		codes = append(codes[:10], fmt.Sprintf("and %d more", len(codes)-10))
	}
	base := serviceURLWithSlug("NOTIFICATIONS_TREASURY_APP_URL", platformOwnerSlug, treasuryAppBase)
	return map[string]any{
		"count":       count,
		"codes":       strings.Join(codes, ", "),
		"review_link": base + "/tax/advisor?section=updates",
	}
}

// platformAlertRecipients is the PLATFORM_ALERT_EMAILS list, cleaned and de-duplicated.
func platformAlertRecipients(list string) []string {
	seen := map[string]bool{}
	var out []string
	for _, addr := range strings.Split(list, ",") {
		addr = strings.TrimSpace(addr)
		key := strings.ToLower(addr)
		if !sendableEmail(addr) || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, addr)
	}
	return out
}

// sendTaxProposalsNotice emails the platform alert list. It never falls back to a tenant contact:
// the proposals are platform business. An empty list (alerts switched off) sends nothing.
func sendTaxProposalsNotice(ctx context.Context, nc *nats.Conn, cfg *config.Config, mapping treasuryNotificationMapping, payload map[string]any, tenantID, aggregateID string, logg *zap.Logger) error {
	to := platformAlertRecipients(cfg.Events.PlatformAlertEmails)
	if len(to) == 0 {
		logg.Info("tax proposals notice: PLATFORM_ALERT_EMAILS is empty, nothing sent")
		return nil
	}
	if aggregateID == "" {
		aggregateID = uuid.NewString()
	}
	msg := messaging.Message{
		TenantID:       tenantID, // treasury's platform tenant
		Channel:        "email",
		TemplateID:     mapping.TemplateID,
		SenderScope:    messaging.SenderScopePlatform,
		Target:         messaging.TargetPlatformAdmin,
		To:             to,
		Data:           mapping.DataBuilder(payload, ""),
		Metadata:       map[string]any{"subject": mapping.EmailSubject},
		RequestID:      uuid.NewString(),
		IdempotencyKey: "treasury-tax-proposals-" + aggregateID,
		QueuedAt:       time.Now(),
	}
	if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
		return err
	}
	logg.Info("tax proposals notice dispatched", zap.Int("recipients", len(to)))
	return nil
}
