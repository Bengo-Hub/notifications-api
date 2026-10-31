package credits

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
)

// SendGate is the source-side entitlement check for SMS and WhatsApp (messaging.ChannelGate). It
// asks subscriptions-api once per tenant and service (Check, which never charges) and remembers
// the answer for ttl, so a busy tenant costs one call a minute, not one per event. A tenant with
// no plan, bundle or credit for the channel never has a message queued for it.
//
// Platform-scope sends are never gated (the platform pays for them). When the answer is unknown
// (subscriptions-api unreachable, tenant not resolvable) the message is queued and the worker's
// charge, which fails closed, decides.
type SendGate struct {
	client  *Client
	resolve func(ctx context.Context, tenant string) (uuid.UUID, bool)
	ttl     time.Duration
	log     *zap.Logger
	now     func() time.Time

	mu      sync.Mutex
	answers map[gateKey]gateAnswer
}

type gateKey struct {
	tenant  uuid.UUID
	service string
}

type gateAnswer struct {
	reason  string
	expires time.Time
}

// maxGateAnswers bounds the cache; expired answers are swept when it is reached.
const maxGateAnswers = 10000

// NewSendGate builds the gate. resolve maps a message's tenant (UUID or slug) to its UUID.
func NewSendGate(client *Client, resolve func(ctx context.Context, tenant string) (uuid.UUID, bool), ttl time.Duration, log *zap.Logger) *SendGate {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &SendGate{client: client, resolve: resolve, ttl: ttl, log: log, now: time.Now, answers: map[gateKey]gateAnswer{}}
}

// ServiceForChannel is the credits service a channel spends, "" for channels that are not paid.
func ServiceForChannel(channel string) string {
	switch strings.ToLower(channel) {
	case "sms":
		return ServiceSMS
	case "whatsapp":
		return ServiceWhatsApp
	}
	return ""
}

// Blocked implements messaging.ChannelGate.
func (g *SendGate) Blocked(ctx context.Context, msg messaging.Message) string {
	service := ServiceForChannel(msg.Channel)
	if service == "" || g == nil || g.client == nil || msg.EffectiveSenderScope() == messaging.SenderScopePlatform {
		return ""
	}
	tenantID, ok := uuid.Nil, false
	if g.resolve != nil {
		tenantID, ok = g.resolve(ctx, msg.TenantID)
	}
	if !ok || tenantID == uuid.Nil {
		return ""
	}
	key := gateKey{tenant: tenantID, service: service}
	now := g.now()
	g.mu.Lock()
	if a, hit := g.answers[key]; hit && now.Before(a.expires) {
		g.mu.Unlock()
		return a.reason
	}
	g.mu.Unlock()

	dec, err := g.client.Check(ctx, tenantID, service, 1, false)
	if err != nil {
		if g.log != nil {
			g.log.Debug("send gate: entitlement unknown, leaving the decision to the worker",
				zap.String("tenant_id", tenantID.String()), zap.String("service", service), zap.Error(err))
		}
		return ""
	}
	reason := ""
	if !dec.Allowed {
		reason = RefusalMessage(service, dec.Reason)
		if g.log != nil {
			g.log.Debug("send gate: blocked at source", zap.String("tenant_id", tenantID.String()),
				zap.String("service", service), zap.String("reason", dec.Reason), zap.String("template", msg.TemplateID))
		}
	}
	g.mu.Lock()
	if len(g.answers) >= maxGateAnswers {
		for k, a := range g.answers {
			if !now.Before(a.expires) {
				delete(g.answers, k)
			}
		}
	}
	g.answers[key] = gateAnswer{reason: reason, expires: now.Add(g.ttl)}
	g.mu.Unlock()
	return reason
}
