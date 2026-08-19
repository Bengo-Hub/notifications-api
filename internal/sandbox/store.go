// Package sandbox holds a message the developer "sent" while calling notifications-api
// with a sandbox App token — never a real provider call, never a row in the real
// production tables. Backed by Redis so it's genuinely ephemeral: capped list length +
// TTL means old sandbox traffic disappears on its own, matching the "test, then it's
// gone once you move to production" expectation.
package sandbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// TTL is how long a tenant's sandbox message history survives without new activity —
// each write refreshes it, so an actively-testing developer never loses their recent
// history mid-session, but an abandoned sandbox integration cleans itself up.
const TTL = 48 * time.Hour

// MaxPerTenant caps the stored list length so a runaway test loop can't grow this key
// without bound — oldest entries fall off first.
const MaxPerTenant = 200

// Message is what a developer sees back when listing their sandbox activity.
type Message struct {
	ID            string         `json:"id"`
	Channel       string         `json:"channel"`
	Template      string         `json:"template"`
	To            []string       `json:"to"`
	Data          map[string]any `json:"data,omitempty"`
	Status        string         `json:"status"`
	SentAt        time.Time      `json:"sent_at"`
	SimulatedNote string         `json:"note"`
}

type Store struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func key(tenantID string) string {
	return "sandbox:messages:" + tenantID
}

// Save appends a simulated message for tenantID and refreshes the TTL. Best-effort —
// a Redis error here must never fail the caller's Enqueue request.
func (s *Store) Save(ctx context.Context, tenantID string, msg Message) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	k := key(tenantID)
	pipe := s.rdb.Pipeline()
	pipe.LPush(cctx, k, body)
	pipe.LTrim(cctx, k, 0, MaxPerTenant-1)
	pipe.Expire(cctx, k, TTL)
	_, err = pipe.Exec(cctx)
	return err
}

// List returns tenantID's sandbox messages, most recent first.
func (s *Store) List(ctx context.Context, tenantID string) ([]Message, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := s.rdb.LRange(cctx, key(tenantID), 0, MaxPerTenant-1).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Message, 0, len(raw))
	for _, r := range raw {
		var m Message
		if json.Unmarshal([]byte(r), &m) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}
