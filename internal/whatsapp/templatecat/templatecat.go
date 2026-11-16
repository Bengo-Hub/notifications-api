// Package templatecat keeps the category Meta actually holds each WhatsApp template under, which
// is the category Meta bills. Meta can approve a template we submit as UTILITY under MARKETING, or
// move it later (16 November 2026: 9 of ours), so the manifest alone is not enough to bill sends
// right or to notice a move. The store lives in Redis, shared by the API (webhook and sweep write
// it) and the worker (its Cache reads it once a minute to price each send).
package templatecat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyByWABA      = "wa:tplcat:waba" // hash: "<waba>|<name>" -> category
	keyByName      = "wa:tplcat:name" // hash: name -> the dearest category across WABAs
	keySendStatus  = "wa:sendstatus:" // + Meta message id -> SendStatus JSON
	keyClaim       = "wa:tplcat:claim:"
	sendStatusTTL  = time.Hour
	defaultClaimTT = 24 * time.Hour
)

// backend is the part of Redis the store uses.
type backend interface {
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	HSet(ctx context.Context, key, field, value string) error
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
}

type redisBackend struct{ rdb redis.UniversalClient }

func (r redisBackend) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return r.rdb.HGetAll(ctx, key).Result()
}
func (r redisBackend) HSet(ctx context.Context, key, field, value string) error {
	return r.rdb.HSet(ctx, key, field, value).Err()
}
func (r redisBackend) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return r.rdb.SetNX(ctx, key, value, ttl).Result()
}
func (r redisBackend) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := r.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return v, err == nil, err
}
func (r redisBackend) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.rdb.Set(ctx, key, value, ttl).Err()
}

// Store reads and writes Meta's template categories and recent send statuses.
type Store struct{ b backend }

// NewStore builds the store on Redis; nil gives nil, and every method of a nil store is a no-op.
func NewStore(rdb redis.UniversalClient) *Store {
	if rdb == nil {
		return nil
	}
	return &Store{b: redisBackend{rdb: rdb}}
}

func newStoreWith(b backend) *Store { return &Store{b: b} }

// price ranks categories by what Meta charges, so a name on two WABAs bills as the dearer one.
func price(cat string) int {
	switch cat {
	case "MARKETING", "MARKETING_LITE":
		return 3
	case "UTILITY":
		return 2
	case "AUTHENTICATION", "AUTHENTICATION_INTERNATIONAL":
		return 1
	case "SERVICE":
		return 0
	}
	return 2
}

// Set records the category Meta holds a template under on one WABA. It returns the category
// recorded before for that WABA and whether this is a change from a known one (a first record
// is not a change: there is nothing to compare with).
func (s *Store) Set(ctx context.Context, wabaID, name, category string) (previous string, changed bool, err error) {
	if s == nil || name == "" || category == "" {
		return "", false, nil
	}
	category = strings.ToUpper(category)
	field := wabaID + "|" + name
	all, err := s.b.HGetAll(ctx, keyByWABA)
	if err != nil {
		return "", false, err
	}
	previous = all[field]
	if err := s.b.HSet(ctx, keyByWABA, field, category); err != nil {
		return previous, false, err
	}
	all[field] = category
	// The name's billing category: the dearest across the WABAs that hold it.
	best := ""
	for f, c := range all {
		if f[strings.Index(f, "|")+1:] != name {
			continue
		}
		if best == "" || price(c) > price(best) {
			best = c
		}
	}
	if err := s.b.HSet(ctx, keyByName, name, best); err != nil {
		return previous, false, err
	}
	return previous, previous != "" && previous != category, nil
}

// All returns each template name's billing category.
func (s *Store) All(ctx context.Context) (map[string]string, error) {
	if s == nil {
		return map[string]string{}, nil
	}
	return s.b.HGetAll(ctx, keyByName)
}

// Claim reports whether this caller is the first to claim key within ttl (0: a day). Used so
// that only one replica alerts on a webhook Meta may send twice, or runs a sweep.
func (s *Store) Claim(ctx context.Context, key string, ttl time.Duration) bool {
	if s == nil {
		return true
	}
	if ttl <= 0 {
		ttl = defaultClaimTT
	}
	ok, err := s.b.SetNX(ctx, keyClaim+key, "1", ttl)
	return err == nil && ok
}

// SendStatus is the latest status Meta's webhook reported for one sent message.
type SendStatus struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"` // sent | delivered | read | failed
	Code      int    `json:"code,omitempty"`
	Title     string `json:"title,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// statusRank orders statuses: Meta can report "sent" after "delivered", and a failure is final.
func statusRank(s string) int {
	switch s {
	case "failed":
		return 9
	case "read":
		return 3
	case "delivered":
		return 2
	case "sent":
		return 1
	}
	return 0
}

// RecordSendStatus keeps a send's status for an hour, never letting an earlier stage replace a
// later one. Best effort.
func (s *Store) RecordSendStatus(ctx context.Context, st SendStatus) {
	if s == nil || st.MessageID == "" {
		return
	}
	if cur, ok := s.SendStatus(ctx, st.MessageID); ok && statusRank(cur.Status) >= statusRank(st.Status) {
		return
	}
	b, _ := json.Marshal(st)
	_ = s.b.Set(ctx, keySendStatus+st.MessageID, string(b), sendStatusTTL)
}

// SendStatus returns the latest status recorded for a Meta message id.
func (s *Store) SendStatus(ctx context.Context, messageID string) (SendStatus, bool) {
	if s == nil || messageID == "" {
		return SendStatus{}, false
	}
	v, ok, err := s.b.Get(ctx, keySendStatus+messageID)
	if err != nil || !ok {
		return SendStatus{}, false
	}
	var st SendStatus
	if json.Unmarshal([]byte(v), &st) != nil {
		return SendStatus{}, false
	}
	return st, true
}

// Change is one change Meta made to a template on a WhatsApp Business Account we send from: a
// category move (Kind "category") or a review outcome (Kind "status").
type Change struct {
	Kind     string // "category" or "status"
	WABAID   string
	Name     string
	Language string
	// Category move.
	PreviousCategory string
	NewCategory      string
	// Review outcome: APPROVED, REJECTED, PAUSED, DISABLED, FLAGGED, PENDING_DELETION ...
	Status string
	Reason string
	// Source says how we learned it: "webhook" (Meta told us) or "sweep" (the periodic check).
	Source string
}

// Alerter tells ops about a Change (the events subscriber, by email and Slack).
type Alerter interface {
	AlertWhatsAppTemplateChange(ctx context.Context, c Change)
}

// Cache is an in-process copy of the store's name categories, so pricing a send never waits on
// Redis. Refresh it on a timer (Run).
type Cache struct {
	store *Store
	m     atomic.Pointer[map[string]string]
	once  sync.Once
}

// NewCache builds a cache over store (nil store: it never answers).
func NewCache(store *Store) *Cache { return &Cache{store: store} }

// Refresh reloads every category from the store.
func (c *Cache) Refresh(ctx context.Context) error {
	if c == nil || c.store == nil {
		return nil
	}
	all, err := c.store.All(ctx)
	if err != nil {
		return err
	}
	c.m.Store(&all)
	return nil
}

// Run refreshes now and then every interval until ctx ends.
func (c *Cache) Run(ctx context.Context, every time.Duration) {
	if c == nil || c.store == nil {
		return
	}
	c.once.Do(func() {
		_ = c.Refresh(ctx)
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_ = c.Refresh(ctx)
				}
			}
		}()
	})
}

// Lookup returns the category Meta bills the named template under, when known.
func (c *Cache) Lookup(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	m := c.m.Load()
	if m == nil {
		return "", false
	}
	cat, ok := (*m)[name]
	return cat, ok && cat != ""
}
