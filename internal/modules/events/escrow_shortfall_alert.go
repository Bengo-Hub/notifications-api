package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	tenantmod "github.com/bengobox/notifications-api/internal/modules/tenant"
)

// Escrow reconciliation alerts (owner, 13 November 2026). treasury-api's hourly escrow
// reconciliation (escrow/reconcile.go, ReconcileAll) publishes treasury.escrow.reconciliation_shortfall
// once when a tenant's Team wallet first holds less than its pots owe (an incident opens), and
// treasury.escrow.reconciliation_recovered once when the wallet covers the pots again. While an
// incident is open treasury refuses every Team wallet withdrawal and skips the daily commission sweep,
// so the shortfall needs a person: ops get it by email, Slack and WhatsApp (sendOpsAlert). The recovery
// is a routine notice: email and Slack only.
//
// Tenants in platform_root mode share one PayHero root wallet, and a shortfall there opens an
// incident on every tenant of the account, each published as its own event with scope
// shared_account, vendor_id and shared_owed. Those siblings are gathered for escrowSharedWindow and
// sent as one alert naming every tenant.
//
// Dedupe: one alert per incident, not one per hourly check or redelivery. An own-account incident is
// keyed on its incident_id (tenant plus day when the id is missing), a shared account on its vendor
// id; the key holds the last state alerted (short or recovered) and only a change sends. The state
// is kept in Redis when there is one, else in this process.
const (
	escrowShortfallTemplate = "platform/escrow_shortfall"
	escrowRecoveredTemplate = "platform/escrow_recovered"
	// escrowAlertStateTTL is how long an alerted state is remembered.
	escrowAlertStateTTL  = 14 * 24 * time.Hour
	escrowAlertKeyPrefix = "notif:escrow-alert:"
	escrowScopeShared    = "shared_account"
)

// escrowSharedWindow is how long the siblings of a shared account incident are gathered. treasury
// publishes them in one reconciliation pass, so they arrive within seconds; the window stays well
// inside the subscription's 30 second ack wait. A variable so tests need not wait.
var escrowSharedWindow = 10 * time.Second

// escrowEvent is the shared-events envelope treasury publishes (payments.OutboxEventPublisher).
type escrowEvent struct {
	TenantID string        `json:"tenant_id"`
	Payload  escrowPayload `json:"payload"`
}

// escrowPayload is the payload of both events. Amounts are decimal strings with two places. The
// payload carries no personal data.
type escrowPayload struct {
	IncidentID    string `json:"incident_id"`
	WalletBalance string `json:"wallet_balance"`
	PotsTotal     string `json:"pots_total"`
	InFlight      string `json:"in_flight"`
	Currency      string `json:"currency"`
	AsOf          string `json:"as_of"`
	Shortfall     string `json:"shortfall"`
	Scope         string `json:"scope"`
	VendorID      int64  `json:"vendor_id"`
	SharedOwed    string `json:"shared_owed"`
}

func (p escrowPayload) shared() bool { return p.Scope == escrowScopeShared && p.VendorID > 0 }

// escrowTenant is one tenant named in an alert, with its own pot figures.
type escrowTenant struct {
	ID       string
	Name     string
	Pots     string
	InFlight string
}

// escrowAlerts holds the shared-account batches and the dedupe state.
type escrowAlerts struct {
	mu      sync.Mutex
	batches map[string]*escrowBatch
	mem     map[string]escrowMemState
	rdb     *redis.Client
}

type escrowMemState struct {
	state   string
	expires time.Time
}

// escrowBatch is one shared account's events gathered inside the window.
type escrowBatch struct {
	recovered bool
	payload   escrowPayload
	tenants   []escrowTenant
	msgs      []*nats.Msg
}

// WithCache sets the Redis client the escrow alert dedupe keeps its state in. Without one the state
// lives in this process.
func (s *Subscriber) WithCache(rdb *redis.Client) *Subscriber {
	s.escrow.rdb = rdb
	return s
}

func (s *Subscriber) handleEscrowShortfall(msg *nats.Msg) { s.handleEscrowEvent(msg, false) }

func (s *Subscriber) handleEscrowRecovered(msg *nats.Msg) { s.handleEscrowEvent(msg, true) }

func (s *Subscriber) handleEscrowEvent(msg *nats.Msg, recovered bool) {
	var e escrowEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn("escrow reconciliation alert: unmarshal failed", zap.Error(err))
		_ = msg.Ack() // a malformed payload will never parse
		return
	}
	p := e.Payload
	t := escrowTenant{ID: e.TenantID, Pots: p.PotsTotal, InFlight: p.InFlight}
	if !p.shared() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		s.sendEscrowAlert(ctx, recovered, p, []escrowTenant{t})
		_ = msg.Ack()
		return
	}
	key := strconv.FormatInt(p.VendorID, 10) + ":" + escrowKind(recovered)
	s.escrow.mu.Lock()
	defer s.escrow.mu.Unlock()
	if s.escrow.batches == nil {
		s.escrow.batches = map[string]*escrowBatch{}
	}
	b := s.escrow.batches[key]
	if b == nil {
		b = &escrowBatch{recovered: recovered, payload: p}
		s.escrow.batches[key] = b
		time.AfterFunc(escrowSharedWindow, func() { s.flushEscrowBatch(key) })
	}
	if !b.has(t.ID) {
		b.tenants = append(b.tenants, t)
	}
	b.msgs = append(b.msgs, msg)
}

func (b *escrowBatch) has(id string) bool {
	for _, t := range b.tenants {
		if t.ID == id {
			return true
		}
	}
	return false
}

// flushEscrowBatch sends one alert for a shared account's gathered events, then acks them.
func (s *Subscriber) flushEscrowBatch(key string) {
	s.escrow.mu.Lock()
	b := s.escrow.batches[key]
	delete(s.escrow.batches, key)
	s.escrow.mu.Unlock()
	if b == nil {
		return
	}
	for _, m := range b.msgs {
		_ = m.InProgress()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	s.sendEscrowAlert(ctx, b.recovered, b.payload, b.tenants)
	for _, m := range b.msgs {
		_ = m.Ack()
	}
}

// sendEscrowAlert sends the ops alert unless this incident's state was already alerted.
func (s *Subscriber) sendEscrowAlert(ctx context.Context, recovered bool, p escrowPayload, tenants []escrowTenant) {
	tid := ""
	if len(tenants) > 0 {
		tid = tenants[0].ID
	}
	key := escrowAlertKey(p, tid, time.Now())
	if !s.escrow.transition(ctx, key, escrowKind(recovered)) {
		s.log.Info("escrow reconciliation alert already sent", zap.String("key", key), zap.Bool("recovered", recovered))
		return
	}
	for i := range tenants {
		tenants[i].Name = s.tenantLabel(ctx, tenants[i].ID)
	}
	s.sendOpsAlert(ctx, escrowOpsAlert(recovered, p, tenants))
	s.log.Info("escrow reconciliation alert dispatched", zap.String("key", key), zap.Bool("recovered", recovered),
		zap.Int("tenants", len(tenants)), zap.Bool("shared", p.shared()))
}

func escrowKind(recovered bool) string {
	if recovered {
		return "recovered"
	}
	return "short"
}

// escrowAlertKey names the incident an event belongs to: the shared account, the incident id, or
// the tenant and day when treasury sent no id.
func escrowAlertKey(p escrowPayload, tenantID string, now time.Time) string {
	switch {
	case p.shared():
		return escrowAlertKeyPrefix + "shared:" + strconv.FormatInt(p.VendorID, 10)
	case strings.TrimSpace(p.IncidentID) != "":
		return escrowAlertKeyPrefix + "incident:" + strings.TrimSpace(p.IncidentID)
	}
	return escrowAlertKeyPrefix + "tenant:" + tenantID + ":" + now.UTC().Format("2006-01-02")
}

// transition records state under key and reports whether it differs from the state last alerted.
func (a *escrowAlerts) transition(ctx context.Context, key, state string) bool {
	if a.rdb != nil {
		prev, err := a.rdb.SetArgs(ctx, key, state, redis.SetArgs{Get: true, TTL: escrowAlertStateTTL}).Result()
		if err == nil || errors.Is(err, redis.Nil) {
			return prev != state
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mem == nil {
		a.mem = map[string]escrowMemState{}
	}
	now := time.Now()
	prev := a.mem[key]
	if now.After(prev.expires) {
		prev.state = ""
	}
	a.mem[key] = escrowMemState{state: state, expires: now.Add(escrowAlertStateTTL)}
	return prev.state != state
}

// tenantLabel names a tenant by its name, else its slug, from the local tenant table (filled from
// auth-api when missing). The tenant id is the last resort; no personal data is read.
func (s *Subscriber) tenantLabel(ctx context.Context, id string) string {
	uid, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil || s.entClient == nil {
		return id
	}
	t, err := s.entClient.Tenant.Get(ctx, uid)
	if (err != nil || t == nil) && s.authAPI != "" {
		t, err = tenantmod.NewSyncer(s.entClient, s.authAPI).SyncTenantByID(ctx, uid)
	}
	if err != nil || t == nil {
		return id
	}
	return firstNonEmpty(t.Name, t.Slug, id)
}

// escrowOpsAlert builds the ops alert for a shortfall or a recovery. The shortfall goes to WhatsApp
// too; the recovery stays on email and Slack.
func escrowOpsAlert(recovered bool, p escrowPayload, tenants []escrowTenant) opsAlert {
	data := escrowAlertData(recovered, p, tenants)
	who := data["tenant_list"].(string)
	where := who + " (own PayHero account)"
	if p.shared() {
		where = fmt.Sprintf("shared PayHero account %d (%s)", p.VendorID, who)
	}
	tid := ""
	if len(tenants) > 0 {
		tid = tenants[0].ID
	}
	owed := data["owed"].(string)
	if recovered {
		text := fmt.Sprintf("Escrow recovered on %s: the wallet holds %s against %s owed. Withdrawals and the commission sweep run again.",
			where, data["wallet_balance"], owed)
		return opsAlert{TenantID: tid, Template: escrowRecoveredTemplate, Subject: "Escrow recovered: " + escrowSubjectWho(p, who),
			Data: data, Text: text, From: "Codevertex Treasury", SlackPrefix: ":white_check_mark: "}
	}
	text := fmt.Sprintf("Escrow shortfall on %s: the wallet holds %s against %s owed, %s short. Withdrawals and the commission sweep are stopped. Top up or investigate the wallet; the hourly reconciliation recovers by itself.",
		where, data["wallet_balance"], owed, data["shortfall"])
	subject := fmt.Sprintf("Escrow shortfall: %s is %s short", escrowSubjectWho(p, who), data["shortfall"])
	return opsAlert{TenantID: tid, Template: escrowShortfallTemplate, Subject: subject, Data: data, Text: text,
		From: "Codevertex Treasury", SlackPrefix: ":rotating_light: ", WhatsApp: true}
}

func escrowSubjectWho(p escrowPayload, who string) string {
	if p.shared() {
		return fmt.Sprintf("shared PayHero account %d", p.VendorID)
	}
	return who + " wallet"
}

// escrowAlertData builds the email data. Optional figures are left out when treasury did not send
// them, so the template's {{ if }} guards drop the line.
func escrowAlertData(recovered bool, p escrowPayload, tenants []escrowTenant) map[string]any {
	currency := firstNonEmpty(p.Currency, "KES")
	names := make([]string, 0, len(tenants))
	rows := make([]map[string]any, 0, len(tenants))
	for _, t := range tenants {
		name := firstNonEmpty(t.Name, t.ID, "unknown tenant")
		names = append(names, name)
		row := map[string]any{"name": name}
		if strings.TrimSpace(t.Pots) != "" {
			row["pots"] = hadiaAmount(t.Pots, currency)
		}
		if escrowNonZero(t.InFlight) {
			row["in_flight"] = hadiaAmount(t.InFlight, currency)
		}
		rows = append(rows, row)
	}
	data := map[string]any{
		"recovered":    recovered,
		"shared":       p.shared(),
		"tenants":      rows,
		"tenant_count": len(rows),
		"tenant_list":  firstNonEmpty(strings.Join(names, ", "), "unknown tenant"),
		"currency":     currency,
	}
	// owed is what the wallet must cover: every sharing tenant's pots for a shared account, else the
	// tenant's pots plus releases and refunds in flight.
	owed := p.PotsTotal
	if p.shared() {
		data["vendor_id"] = p.VendorID
		if strings.TrimSpace(p.SharedOwed) != "" {
			owed = p.SharedOwed
			data["shared_owed"] = hadiaAmount(p.SharedOwed, currency)
		}
	} else if pots, err := strconv.ParseFloat(strings.TrimSpace(p.PotsTotal), 64); err == nil {
		if inFlight, err := strconv.ParseFloat(strings.TrimSpace(p.InFlight), 64); err == nil {
			owed = strconv.FormatFloat(pots+inFlight, 'f', 2, 64)
		}
	}
	data["owed"] = hadiaAmount(owed, currency)
	data["wallet_balance"] = hadiaAmount(p.WalletBalance, currency)
	if !recovered && strings.TrimSpace(p.Shortfall) != "" {
		data["shortfall"] = hadiaAmount(p.Shortfall, currency)
	} else if !recovered {
		data["shortfall"] = "an unknown amount"
	}
	if strings.TrimSpace(p.AsOf) != "" {
		data["as_of"] = readableTime(p.AsOf)
	}
	if strings.TrimSpace(p.IncidentID) != "" {
		data["incident_id"] = strings.TrimSpace(p.IncidentID)
	}
	return data
}

// escrowNonZero reports a decimal string that is set and not zero.
func escrowNonZero(v string) bool {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return err == nil && f != 0
}
