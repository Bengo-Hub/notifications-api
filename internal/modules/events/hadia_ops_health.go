package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/moneyfmt"
)

// Hadia's platform health reports (hadia-api internal/modules/opsreport, Sprint 08 L88 and L89).
// hadia-api publishes hadia.ops.health_daily every morning and hadia.ops.health_weekly every Monday,
// both from 09:00 East Africa time, with responders ["platform_ops"]. They are for platform ops, not
// Hadia's own staff, so they take the ops alert path (sendOpsAlert): email to the platform alert list
// and the Slack webhook in auth-api's platform_alert_channels. A daily report that needs a person also
// goes to the ops WhatsApp numbers; a quiet day and the weekly report do not, so WhatsApp stays an
// alarm. The hadia worker consumer (cmd/worker/hadia_consumer.go) acks these events as unmapped.
const (
	hadiaHealthDailyTemplate  = "platform/hadia_health_daily"
	hadiaHealthWeeklyTemplate = "platform/hadia_health_weekly"
	// hadiaOutboxStaleSeconds is how old the oldest unsent outbox event may be before the daily report
	// calls it out: ten minutes is far past a normal drain, so the poller or NATS is stuck.
	hadiaOutboxStaleSeconds = 600
	// hadiaOpsResponder is the responder hadia-api names for these reports.
	hadiaOpsResponder = "platform_ops"
)

// hadiaHealthEvent is the envelope of both reports. Every field is a count or a sum; the payload
// carries no personal data.
type hadiaHealthEvent struct {
	TenantID string             `json:"tenant_id"`
	Payload  hadiaHealthPayload `json:"payload"`
}

// hadiaHealthPayload holds the fields of both reports: the daily ones (opsreport.dailyFigures), the
// weekly ones (opsreport.weeklyFigures) and responders and generated_at on both. ConsumerLag is the
// waiting messages by durable name, nil when hadia-api could not read it.
type hadiaHealthPayload struct {
	TenantID                 string            `json:"tenant_id"`
	Day                      string            `json:"day"`
	Reconcile                hadiaReconcile    `json:"reconcile"`
	FailedPayouts24h         int               `json:"failed_payouts_24h"`
	FailedPayoutsApproximate bool              `json:"failed_payouts_24h_approximate"`
	FailedPayoutsBasis       string            `json:"failed_payouts_24h_basis"`
	Cases                    hadiaCaseCounts   `json:"cases"`
	Outbox                   hadiaOutbox       `json:"outbox"`
	ConsumerLag              map[string]uint64 `json:"consumer_lag"`
	WeekFrom                 string            `json:"week_from"`
	WeekTo                   string            `json:"week_to"`
	Payments                 hadiaPayments     `json:"payments"`
	Payouts                  hadiaPayouts      `json:"payouts"`
	OpenDefects              map[string]int    `json:"open_defects"`
	Currency                 string            `json:"currency"`
	CurrencyScope            string            `json:"currency_scope"`
	Responders               []string          `json:"responders"`
	GeneratedAt              string            `json:"generated_at"`
}

type hadiaReconcile struct {
	RegistriesDiffering int `json:"registries_differing"`
}

type hadiaCaseCounts struct {
	Open       int `json:"open"`
	InProgress int `json:"in_progress"`
	Overdue    int `json:"overdue"`
	Urgent     int `json:"urgent"`
}

type hadiaOutbox struct {
	Pending              int `json:"pending"`
	OldestPendingSeconds int `json:"oldest_pending_seconds"`
	Failed               int `json:"failed"`
}

type hadiaPayments struct {
	Count int    `json:"count"`
	Value string `json:"value"`
}

type hadiaPayouts struct {
	Count int    `json:"count"`
	Gross string `json:"gross"`
	Net   string `json:"net"`
}

// forOps reports whether platform ops is a responder. A report naming nobody still goes to ops.
func (p hadiaHealthPayload) forOps() bool {
	if len(p.Responders) == 0 {
		return true
	}
	for _, r := range p.Responders {
		if r == hadiaOpsResponder {
			return true
		}
	}
	return false
}

func (s *Subscriber) handleHadiaHealthDaily(msg *nats.Msg) {
	e, ok := s.decodeHadiaHealth(msg, "hadia_health_daily")
	if !ok {
		return
	}
	data, attention := hadiaDailyData(e.Payload)
	subject, text := hadiaDailySummary(e.Payload, attention)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	prefix := ":white_check_mark: "
	if len(attention) > 0 {
		prefix = ":warning: "
	}
	s.sendOpsAlert(ctx, opsAlert{TenantID: firstNonEmpty(e.TenantID, e.Payload.TenantID), Template: hadiaHealthDailyTemplate,
		Subject: subject, Data: data, Text: text, From: "Hadia", SlackPrefix: prefix, WhatsApp: len(attention) > 0})
	s.log.Info("hadia daily health report dispatched", zap.String("day", e.Payload.Day), zap.Int("attention", len(attention)))
	_ = msg.Ack()
}

func (s *Subscriber) handleHadiaHealthWeekly(msg *nats.Msg) {
	e, ok := s.decodeHadiaHealth(msg, "hadia_health_weekly")
	if !ok {
		return
	}
	data := hadiaWeeklyData(e.Payload)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	s.sendOpsAlert(ctx, opsAlert{TenantID: firstNonEmpty(e.TenantID, e.Payload.TenantID), Template: hadiaHealthWeeklyTemplate,
		Subject: hadiaWeeklySubject(e.Payload), Data: data, Text: hadiaWeeklyText(e.Payload, data), From: "Hadia", SlackPrefix: ":bar_chart: "})
	s.log.Info("hadia weekly health report dispatched", zap.String("week_from", e.Payload.WeekFrom))
	_ = msg.Ack()
}

// decodeHadiaHealth reads a report; it acks and answers false for one that will never parse or does
// not name platform ops.
func (s *Subscriber) decodeHadiaHealth(msg *nats.Msg, name string) (hadiaHealthEvent, bool) {
	var e hadiaHealthEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn(name+": unmarshal failed", zap.Error(err))
		_ = msg.Ack()
		return e, false
	}
	if !e.Payload.forOps() {
		_ = msg.Ack()
		return e, false
	}
	return e, true
}

// hadiaDailyData builds the daily email's data and the list of things that need a person.
func hadiaDailyData(p hadiaHealthPayload) (map[string]any, []string) {
	var attention []string
	if n := p.Reconcile.RegistriesDiffering; n > 0 {
		attention = append(attention, countOf(n, "registry", "registries")+" with a gift log that differs from treasury")
	}
	if n := p.FailedPayouts24h; n > 0 {
		line := countOf(n, "payout", "payouts") + " failed in the last 24 hours"
		if p.FailedPayoutsApproximate {
			line += " (approximate)"
		}
		attention = append(attention, line)
	}
	if n := p.Cases.Urgent; n > 0 {
		attention = append(attention, countOf(n, "urgent case", "urgent cases")+" open")
	}
	if n := p.Cases.Overdue; n > 0 {
		attention = append(attention, countOf(n, "case", "cases")+" past the due time")
	}
	if n := p.Outbox.Failed; n > 0 {
		attention = append(attention, countOf(n, "outbox event", "outbox events")+" failed to publish")
	}
	if p.Outbox.Pending > 0 && p.Outbox.OldestPendingSeconds >= hadiaOutboxStaleSeconds {
		attention = append(attention, "the oldest unsent outbox event is "+readableSeconds(p.Outbox.OldestPendingSeconds)+" old")
	}
	data := map[string]any{
		"day":                  firstNonEmpty(p.Day, "today"),
		"attention":            attention,
		"registries_differing": p.Reconcile.RegistriesDiffering,
		"failed_payouts":       p.FailedPayouts24h,
		"failed_approximate":   p.FailedPayoutsApproximate,
		"cases_open":           p.Cases.Open,
		"cases_in_progress":    p.Cases.InProgress,
		"cases_overdue":        p.Cases.Overdue,
		"cases_urgent":         p.Cases.Urgent,
		"outbox_pending":       p.Outbox.Pending,
		"outbox_failed":        p.Outbox.Failed,
		"generated_at":         readableTime(p.GeneratedAt),
	}
	if p.Outbox.Pending > 0 && p.Outbox.OldestPendingSeconds > 0 {
		data["outbox_oldest"] = readableSeconds(p.Outbox.OldestPendingSeconds)
	}
	if p.FailedPayoutsApproximate && p.FailedPayoutsBasis != "" {
		data["failed_basis"] = p.FailedPayoutsBasis
	}
	// Consumer lag only when hadia-api read it (nil leaves the section out); busiest first.
	if p.ConsumerLag != nil {
		lag := make([]map[string]any, 0, len(p.ConsumerLag))
		for durable, waiting := range p.ConsumerLag {
			lag = append(lag, map[string]any{"durable": durable, "waiting": waiting})
		}
		sort.Slice(lag, func(i, j int) bool {
			wi, wj := lag[i]["waiting"].(uint64), lag[j]["waiting"].(uint64)
			if wi != wj {
				return wi > wj
			}
			return lag[i]["durable"].(string) < lag[j]["durable"].(string)
		})
		data["lag"] = lag
		data["lag_read"] = true
	}
	return data, attention
}

// hadiaDailySummary is the subject and the one-line text for Slack and WhatsApp (one line: Meta
// refuses a template parameter with a line break).
func hadiaDailySummary(p hadiaHealthPayload, attention []string) (string, string) {
	day := firstNonEmpty(p.Day, "today")
	tail := fmt.Sprintf("Open cases %d (%d urgent, %d overdue), outbox pending %d.", p.Cases.Open, p.Cases.Urgent, p.Cases.Overdue, p.Outbox.Pending)
	if len(attention) == 0 {
		return "Hadia health for " + day + ": all clear", "Hadia health for " + day + ": all clear. " + tail
	}
	subject := "Hadia health for " + day + ": " + countOf(len(attention), "thing needs", "things need") + " attention"
	return subject, "Hadia health for " + day + ": " + strings.Join(attention, "; ") + ". " + tail
}

// hadiaWeeklyData builds the weekly email's data. Money is in the report's one currency (KES).
func hadiaWeeklyData(p hadiaHealthPayload) map[string]any {
	currency := firstNonEmpty(p.Currency, "KES")
	defects := make([]map[string]any, 0, len(p.OpenDefects))
	total := 0
	for kind, n := range p.OpenDefects {
		defects = append(defects, map[string]any{"kind": hadiaKindLabel(kind), "count": n})
		total += n
	}
	sort.Slice(defects, func(i, j int) bool {
		ci, cj := defects[i]["count"].(int), defects[j]["count"].(int)
		if ci != cj {
			return ci > cj
		}
		return defects[i]["kind"].(string) < defects[j]["kind"].(string)
	})
	data := map[string]any{
		"week_from":      readableDate(p.WeekFrom),
		"week_to":        readableDate(p.WeekTo),
		"payments_count": p.Payments.Count,
		"payments_value": hadiaAmount(p.Payments.Value, currency),
		"payouts_count":  p.Payouts.Count,
		"payouts_gross":  hadiaAmount(p.Payouts.Gross, currency),
		"payouts_net":    hadiaAmount(p.Payouts.Net, currency),
		"defects":        defects,
		"defects_total":  total,
		"generated_at":   readableTime(p.GeneratedAt),
	}
	if p.CurrencyScope != "" {
		data["currency_scope"] = p.CurrencyScope
	}
	return data
}

func hadiaWeeklySubject(p hadiaHealthPayload) string {
	if p.WeekFrom == "" || p.WeekTo == "" {
		return "Hadia weekly health report"
	}
	return "Hadia weekly health report: " + readableDate(p.WeekFrom) + " to " + readableDate(p.WeekTo)
}

func hadiaWeeklyText(p hadiaHealthPayload, data map[string]any) string {
	return fmt.Sprintf("Hadia week %s to %s: %s worth %s; %s, %s gross, %s net; %s open.",
		data["week_from"], data["week_to"], countOf(p.Payments.Count, "gift", "gifts"), data["payments_value"],
		countOf(p.Payouts.Count, "payout", "payouts"), data["payouts_gross"], data["payouts_net"],
		countOf(data["defects_total"].(int), "case", "cases"))
}

// hadiaAmount formats a decimal string ("54000.00") as "KES 54,000"; a value that does not parse is
// shown as sent, and an empty one as zero.
func hadiaAmount(v, currency string) string {
	if strings.TrimSpace(v) == "" {
		return moneyfmt.Format(0, currency)
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return currency + " " + v
	}
	return moneyfmt.Format(f, currency)
}

// hadiaKindLabel turns a case kind into words ("unmatched_credit" reads "unmatched credit").
func hadiaKindLabel(kind string) string {
	if kind == "" {
		return "other"
	}
	return strings.ReplaceAll(kind, "_", " ")
}

// countOf words a count with its noun ("1 payout", "3 payouts").
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// readableSeconds words a duration in seconds ("45 seconds", "12 minutes", "3 hours 5 minutes").
func readableSeconds(sec int) string {
	switch {
	case sec < 60:
		return countOf(sec, "second", "seconds")
	case sec < 3600:
		return countOf(sec/60, "minute", "minutes")
	}
	out := countOf(sec/3600, "hour", "hours")
	if m := (sec % 3600) / 60; m > 0 {
		out += " " + countOf(m, "minute", "minutes")
	}
	return out
}

// readableDate turns "2026-11-03" into "3 Nov 2026"; anything else is returned as it is.
func readableDate(v string) string {
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return v
	}
	return t.Format("2 Jan 2006")
}
