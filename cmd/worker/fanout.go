package main

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
	"github.com/bengobox/notifications-api/internal/platform/templates"
)

// channelOrder is the deterministic fan-out order — purely for stable logging, delivery order
// has no functional significance since each channel is an independent Publish.
var channelOrder = []string{"email", "sms", "whatsapp", "push"}

// buildTemplateChannels scans the template directory once at worker startup and groups every
// drafted template file by its ID (e.g. "ordering/order_ready") -> the channels it has a file
// for. Built once, not per-message: the worker's hot path checks this map instead of re-walking
// the filesystem for every event (unlike the settings-UI's on-demand equivalent, which is a rare,
// human-paced request).
func buildTemplateChannels(ctx context.Context, tpl *templates.Loader, logg *zap.Logger) map[string][]string {
	out := make(map[string][]string)
	if tpl == nil {
		return out
	}
	summaries, err := tpl.List(ctx)
	if err != nil {
		logg.Warn("fan-out: failed to list templates for channel availability, fan-out disabled", zap.Error(err))
		return out
	}
	for _, s := range summaries {
		out[s.ID] = append(out[s.ID], s.Channel)
	}
	return out
}

// fanOutTargets resolves which channels ONE notification should actually publish on: the
// channel must (1) have a drafted template, (2) have a recipient contact supplied by the
// caller, and (3) be enabled for this tenant+type (preferences.Gate — the same gate the main
// dispatch loop enforces again downstream, so this is a pre-filter to avoid constructing/
// publishing messages that would just be dropped, not a replacement for it).
func fanOutTargets(ctx context.Context, gate *preferences.Gate, templateChannels map[string][]string, tenantID, templateID string, recipients map[string]string) map[string]string {
	hasTemplate := make(map[string]bool, len(templateChannels[templateID]))
	for _, c := range templateChannels[templateID] {
		hasTemplate[c] = true
	}
	out := make(map[string]string, len(channelOrder))
	for _, ch := range channelOrder {
		to := recipients[ch]
		if to == "" || !hasTemplate[ch] {
			continue
		}
		if gate != nil && !gate.Enabled(ctx, tenantID, templateID, ch) {
			continue
		}
		out[ch] = to
	}
	return out
}

// publishFanOut publishes one messaging.Message per resolved channel target, copying `base` and
// overriding Channel/To/RequestID/QueuedAt — and IdempotencyKey, suffixed per channel so the same
// logical notification on two channels never collides on dedup. Returns the channels actually
// published (for the caller's own success/failure log line); a per-channel publish failure is
// logged here and skipped rather than aborting the other channels.
func publishFanOut(ctx context.Context, nc *nats.Conn, cfg *config.Config, base messaging.Message, targets map[string]string, logg *zap.Logger) []string {
	sent := make([]string, 0, len(targets))
	for _, ch := range channelOrder {
		to, ok := targets[ch]
		if !ok {
			continue
		}
		msg := base
		msg.Channel = ch
		msg.To = []string{to}
		msg.RequestID = uuid.New().String()
		if base.IdempotencyKey != "" {
			msg.IdempotencyKey = base.IdempotencyKey + "-" + ch
		}
		msg.QueuedAt = time.Now()
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Warn("fan-out: publish failed for channel, skipping",
				zap.String("template", base.TemplateID), zap.String("channel", ch), zap.Error(err))
			continue
		}
		sent = append(sent, ch)
	}
	return sent
}
