// Package engagement tells the service that sent a message how the recipient engaged with it.
// MarketFlow's nurture steps go out with their enrollment in the message metadata
// (enrollment_id, step_index, sequence_id). For those messages this package publishes
// notifications.engagement.recorded on the notifications stream when the message is sent or
// fails, when WhatsApp reports it delivered, read (opened) or failed, when the mail server
// bounces it, and when the recipient replies on WhatsApp. MarketFlow turns each event into its
// nurture engagement metrics.
//
// Provider callbacks only carry the provider's message id or the sender's number, so at send
// time the correlation is kept in Redis under both, for CorrelationTTL.
package engagement

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/broadcasts"
)

// Subject carries every engagement event (the notifications.> stream binds it).
const Subject = "notifications.engagement.recorded"

// Event types, the same names MarketFlow's NurtureDeliveryEvent uses.
const (
	Sent      = "sent"
	Delivered = "delivered"
	Opened    = "opened"
	Bounced   = "bounced"
	Replied   = "replied"
	Failed    = "failed"
)

// CorrelationTTL is how long a sent message can still report engagement. Nurture steps are days
// apart and most reads and replies come within a day or two.
const CorrelationTTL = 7 * 24 * time.Hour

const (
	keyByMessage   = "notif:engage:msg:"
	keyByRecipient = "notif:engage:to:"
)

// Correlation identifies the nurture step a message belongs to.
type Correlation struct {
	TenantID     string `json:"tenant_id"`
	EnrollmentID string `json:"enrollment_id"`
	SequenceID   string `json:"sequence_id,omitempty"`
	StepIndex    int    `json:"step_index"`
	Channel      string `json:"channel"`
	Source       string `json:"source,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
}

// FromMessage reads the correlation a message carries. ok is false for any message that is not
// a nurture step (no enrollment_id), or whose tenant is not a UUID.
func FromMessage(msg messaging.Message) (Correlation, bool) {
	enrollment := text(msg.Metadata["enrollment_id"])
	if enrollment == "" {
		return Correlation{}, false
	}
	if _, err := uuid.Parse(enrollment); err != nil {
		return Correlation{}, false
	}
	if _, err := uuid.Parse(msg.TenantID); err != nil {
		return Correlation{}, false
	}
	step, _ := strconv.Atoi(text(msg.Metadata["step_index"]))
	return Correlation{
		TenantID:     msg.TenantID,
		EnrollmentID: enrollment,
		SequenceID:   text(msg.Metadata["sequence_id"]),
		StepIndex:    step,
		Channel:      msg.Channel,
		Source:       text(msg.Metadata["source"]),
		RequestID:    msg.RequestID,
	}, true
}

// text flattens a metadata value (a string in process, a string or number after JSON).
func text(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.Itoa(int(t))
	case int:
		return strconv.Itoa(t)
	}
	return ""
}

// Store keeps correlations for provider callbacks. Redis in production.
type Store interface {
	Put(ctx context.Context, key, value string, ttl time.Duration) error
	Get(ctx context.Context, key string) (string, error)
	Take(ctx context.Context, key string) (string, error)
}

// RedisStore adapts a Redis client.
type RedisStore struct{ RDB redis.UniversalClient }

func (s RedisStore) Put(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.RDB.Set(ctx, key, value, ttl).Err()
}
func (s RedisStore) Get(ctx context.Context, key string) (string, error) {
	return s.RDB.Get(ctx, key).Result()
}
func (s RedisStore) Take(ctx context.Context, key string) (string, error) {
	return s.RDB.GetDel(ctx, key).Result()
}

// PublishFunc sends one event; the default publishes on NATS JetStream.
type PublishFunc func(subject, eventType string, tenantID *uuid.UUID, payload map[string]any)

// Tracker records correlations at send time and publishes engagement events. A nil Tracker, or
// one without a store, does nothing, so callers never need to check.
type Tracker struct {
	store   Store
	publish PublishFunc
	log     *zap.Logger
}

// New builds a tracker over Redis and NATS. Either may be nil: without Redis only sent and
// failed are published, without NATS nothing is.
func New(rdb redis.UniversalClient, nc *nats.Conn, log *zap.Logger) *Tracker {
	if nc == nil {
		return nil
	}
	t := &Tracker{log: log.Named("engagement")}
	if rdb != nil {
		t.store = RedisStore{RDB: rdb}
	}
	t.publish = func(subject, eventType string, tenantID *uuid.UUID, payload map[string]any) {
		broadcasts.PublishEvent(nc, t.log, subject, eventType, tenantID, uuid.New(), payload)
	}
	return t
}

// NewWith builds a tracker over any store and publisher (tests).
func NewWith(store Store, publish PublishFunc, log *zap.Logger) *Tracker {
	return &Tracker{store: store, publish: publish, log: log}
}

// MessageSent publishes "sent" for a nurture message and keeps its correlation so later
// callbacks can find it: under the provider message id (WhatsApp's message id, set by the
// provider in metadata sent_message_id), under the request id (the email Message-ID token a
// bounce carries), and under each recipient's number for a WhatsApp or SMS reply.
func (t *Tracker) MessageSent(ctx context.Context, msg messaging.Message) {
	c, ok := FromMessage(msg)
	if t == nil || !ok {
		return
	}
	providerID := text(msg.Metadata[messaging.MetaSentMessageID])
	t.emit(c, Sent, providerID, nil)
	if t.store == nil {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	put := func(key string) {
		if err := t.store.Put(ctx, key, string(data), CorrelationTTL); err != nil {
			t.log.Warn("engagement: could not keep the correlation", zap.Error(err))
		}
	}
	if providerID != "" {
		put(keyByMessage + providerID)
	}
	if c.RequestID != "" && msg.Channel == "email" {
		put(keyByMessage + c.RequestID)
	}
	if msg.Channel == "whatsapp" || msg.Channel == "sms" {
		for _, to := range msg.To {
			if d := lastNine(to); d != "" {
				put(keyByRecipient + msg.Channel + ":" + d)
			}
		}
	}
}

// MessageFailed publishes "failed" for a nurture message that could not be sent at all.
func (t *Tracker) MessageFailed(msg messaging.Message, reason error) {
	c, ok := FromMessage(msg)
	if t == nil || !ok {
		return
	}
	var meta map[string]any
	if reason != nil {
		meta = map[string]any{"error": truncate(reason.Error(), 300)}
	}
	t.emit(c, Failed, "", meta)
}

// WhatsAppStatus maps a Meta status callback onto the sent message: delivered, read (opened)
// or failed. "sent" is skipped because MessageSent already reported it.
func (t *Tracker) WhatsAppStatus(ctx context.Context, providerMessageID, status string) {
	var event string
	switch status {
	case "delivered":
		event = Delivered
	case "read":
		event = Opened
	case "failed":
		event = Failed
	default:
		return
	}
	if c, ok := t.lookup(ctx, keyByMessage+providerMessageID, false); ok {
		t.emit(c, event, providerMessageID, nil)
	}
}

// EmailBounced reports a permanent bounce of the email whose Message-ID token is token.
func (t *Tracker) EmailBounced(ctx context.Context, token string) {
	if c, ok := t.lookup(ctx, keyByMessage+token, true); ok {
		t.emit(c, Bounced, "", nil)
	}
}

// Replied reports an inbound message from a number that recently got a nurture step on channel.
// The first reply to a step counts; tenantID, when known, must match the step's tenant.
func (t *Tracker) Replied(ctx context.Context, channel, from, tenantID, providerMessageID string) {
	d := lastNine(from)
	if d == "" {
		return
	}
	c, ok := t.lookup(ctx, keyByRecipient+channel+":"+d, false)
	if !ok || (tenantID != "" && tenantID != c.TenantID) {
		return
	}
	if t.store != nil {
		_, _ = t.store.Take(ctx, keyByRecipient+channel+":"+d)
	}
	t.emit(c, Replied, providerMessageID, nil)
}

func (t *Tracker) lookup(ctx context.Context, key string, take bool) (Correlation, bool) {
	if t == nil || t.store == nil || strings.HasSuffix(key, ":") {
		return Correlation{}, false
	}
	get := t.store.Get
	if take {
		get = t.store.Take
	}
	raw, err := get(ctx, key)
	if err != nil || raw == "" {
		return Correlation{}, false
	}
	var c Correlation
	if json.Unmarshal([]byte(raw), &c) != nil || c.EnrollmentID == "" {
		return Correlation{}, false
	}
	return c, true
}

// emit publishes one event in the shape MarketFlow's delivery receiver reads.
func (t *Tracker) emit(c Correlation, eventType, providerMessageID string, meta map[string]any) {
	if t.publish == nil {
		return
	}
	payload := map[string]any{
		"tenant_id":     c.TenantID,
		"enrollment_id": c.EnrollmentID,
		"step_index":    c.StepIndex,
		"channel":       c.Channel,
		"event_type":    eventType,
		"occurred_at":   time.Now().UTC().Format(time.RFC3339),
	}
	if providerMessageID != "" {
		payload["provider_message_id"] = providerMessageID
	}
	m := map[string]any{}
	for k, v := range meta {
		m[k] = v
	}
	if c.SequenceID != "" {
		m["sequence_id"] = c.SequenceID
	}
	if c.RequestID != "" {
		m["request_id"] = c.RequestID
	}
	if c.Source != "" {
		m["source"] = c.Source
	}
	if len(m) > 0 {
		payload["metadata"] = m
	}
	var tid *uuid.UUID
	if id, err := uuid.Parse(c.TenantID); err == nil {
		tid = &id
	}
	t.publish(Subject, "engagement.recorded", tid, payload)
}

// lastNine is the last nine digits of a number, enough to match +254..., 0... and 254... forms.
func lastNine(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) < 9 {
		return ""
	}
	return d[len(d)-9:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
