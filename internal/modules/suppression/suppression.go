// Package suppression keeps the per-sender list of addresses that must not get messages (an
// unsubscribe, a STOP reply, a hard bounce) and signs the unsubscribe links put in marketing
// messages.
package suppression

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/Bengo-Hub/httpware/pii"

	"github.com/bengobox/notifications-api/internal/ent"
	entsuppression "github.com/bengobox/notifications-api/internal/ent/suppression"
)

// Scopes.
const (
	ScopeMarketing = "marketing" // offers and greetings
	ScopeAll       = "all"       // everything except locked security messages
)

// Service reads and writes suppressions.
type Service struct {
	client *ent.Client
	keys   [][]byte // HMAC keys for unsubscribe tokens; the first signs, all verify
	notify Notifier
}

// UnsubscribedEvent is published on notifications.recipient.unsubscribed so the service that
// owns the contact (MarketFlow for customers) can clear its own consent flag. Address is the
// normalised address when it is known (a STOP reply, or a recent broadcast to that address).
type UnsubscribedEvent struct {
	TenantID    string `json:"tenant_id,omitempty"` // the sender; empty = the platform
	Channel     string `json:"channel"`
	AddressHash string `json:"address_hash"`
	Address     string `json:"address,omitempty"`
	Scope       string `json:"scope"`
	Reason      string `json:"reason"`
	Source      string `json:"source"`
}

// Notifier publishes an UnsubscribedEvent. Best-effort: the suppression row is the source of truth.
type Notifier func(ctx context.Context, ev UnsubscribedEvent)

// WithNotifier sets who hears about new opt-outs.
func (s *Service) WithNotifier(n Notifier) *Service {
	s.notify = n
	return s
}

// OptOut records that an address opted out of a sender's marketing (unsubscribe link, STOP
// reply, quick-reply button) and tells the contact's owning service.
func (s *Service) OptOut(ctx context.Context, tenantID *uuid.UUID, channel, address, addressHash, reason, source string) error {
	if addressHash == "" && address != "" {
		addressHash = pii.HashAddress(address)
	}
	if err := s.Add(ctx, tenantID, channel, addressHash, ScopeMarketing, reason, source); err != nil {
		return err
	}
	if s.notify != nil {
		ev := UnsubscribedEvent{Channel: channel, AddressHash: addressHash, Address: address, Scope: ScopeMarketing, Reason: reason, Source: source}
		if tenantID != nil {
			ev.TenantID = tenantID.String()
		}
		s.notify(ctx, ev)
	}
	return nil
}

// NewService builds the service. secrets are the service's encryption keys, current first then
// older ones, so links signed before a key rotation keep working. Each token key is derived from
// its secret, so a secret is never used directly for two purposes.
func NewService(client *ent.Client, secrets ...[]byte) *Service {
	s := &Service{client: client}
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		m := hmac.New(sha256.New, secret)
		m.Write([]byte("notifications-unsubscribe-v1"))
		s.keys = append(s.keys, m.Sum(nil))
	}
	return s
}

// Add records a suppression. Adding one that already exists is not an error.
func (s *Service) Add(ctx context.Context, tenantID *uuid.UUID, channel, addressHash, scope, reason, source string) error {
	if channel == "" || addressHash == "" {
		return errors.New("channel and address are required")
	}
	if scope != ScopeAll {
		scope = ScopeMarketing
	}
	c := s.client.Suppression.Create().
		SetChannel(channel).SetAddressHash(addressHash).
		SetScope(entsuppression.Scope(scope)).SetReason(reason).SetSource(source)
	if tenantID != nil {
		c.SetTenantID(*tenantID)
	}
	if err := c.Exec(ctx); err != nil && !ent.IsConstraintError(err) {
		return err
	}
	return nil
}

// Suppressed returns, for the given address hashes on one channel, those the sender must not
// message. marketing=true also counts marketing-only suppressions. One query per batch.
func (s *Service) Suppressed(ctx context.Context, tenantID *uuid.UUID, channel string, hashes []string, marketing bool) (map[string]bool, error) {
	out := map[string]bool{}
	if len(hashes) == 0 {
		return out, nil
	}
	q := s.client.Suppression.Query().Where(
		entsuppression.Channel(channel),
		entsuppression.AddressHashIn(hashes...),
	)
	if tenantID == nil {
		q = q.Where(entsuppression.TenantIDIsNil())
	} else {
		q = q.Where(entsuppression.TenantID(*tenantID))
	}
	if !marketing {
		q = q.Where(entsuppression.ScopeEQ(entsuppression.ScopeAll))
	}
	rows, err := q.Select(entsuppression.FieldAddressHash).Strings(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range rows {
		out[h] = true
	}
	return out, nil
}

// Remove lifts a suppression (the person opted back in).
func (s *Service) Remove(ctx context.Context, tenantID *uuid.UUID, channel, addressHash string) error {
	q := s.client.Suppression.Delete().Where(entsuppression.Channel(channel), entsuppression.AddressHash(addressHash))
	if tenantID == nil {
		q = q.Where(entsuppression.TenantIDIsNil())
	} else {
		q = q.Where(entsuppression.TenantID(*tenantID))
	}
	_, err := q.Exec(ctx)
	return err
}

// Token is what an unsubscribe link carries: who sent it, on which channel, to which address
// (hashed, so the link never reveals the address), and an optional sender name for the page.
type Token struct {
	TenantID    string `json:"t,omitempty"`
	Channel     string `json:"c"`
	AddressHash string `json:"h"`
	Sender      string `json:"n,omitempty"`
}

// ErrBadToken is returned for a token that is malformed or not signed by this service.
var ErrBadToken = errors.New("invalid unsubscribe link")

// Sign makes the token string for a link. Tokens do not expire: an unsubscribe link in an old
// email must keep working.
func (s *Service) Sign(t Token) string {
	payload, _ := json.Marshal(t)
	p := base64.RawURLEncoding.EncodeToString(payload)
	if len(s.keys) == 0 {
		return ""
	}
	return p + "." + base64.RawURLEncoding.EncodeToString(mac(s.keys[0], p))
}

// Verify checks a token string and returns its contents.
func (s *Service) Verify(token string) (Token, error) {
	p, sig, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return Token{}, ErrBadToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return Token{}, ErrBadToken
	}
	valid := false
	for _, k := range s.keys {
		if hmac.Equal(got, mac(k, p)) {
			valid = true
			break
		}
	}
	if !valid {
		return Token{}, ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Token{}, ErrBadToken
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil || t.Channel == "" || t.AddressHash == "" {
		return Token{}, ErrBadToken
	}
	return t, nil
}

// TokenFor builds the token for a recipient address.
func (s *Service) TokenFor(tenantID *uuid.UUID, channel, address, sender string) string {
	t := Token{Channel: channel, AddressHash: pii.HashAddress(address), Sender: sender}
	if tenantID != nil {
		t.TenantID = tenantID.String()
	}
	return s.Sign(t)
}

// Unsubscribe applies a verified token: the address stops getting this sender's marketing.
// address may be "" (the token only carries a hash); pass it when the caller can look it up.
func (s *Service) Unsubscribe(ctx context.Context, t Token, address, source string) error {
	var tid *uuid.UUID
	if t.TenantID != "" {
		id, err := uuid.Parse(t.TenantID)
		if err != nil {
			return ErrBadToken
		}
		tid = &id
	}
	return s.OptOut(ctx, tid, t.Channel, address, t.AddressHash, "unsubscribe", source)
}

func mac(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// IsStopReply reports whether an inbound SMS or WhatsApp text (or quick-reply payload) asks to
// stop: STOP, UNSUBSCRIBE, STOP PROMOTIONS, or the Swahili ACHA / SITISHA, any case, alone.
func IsStopReply(text string) bool {
	t := strings.ToUpper(strings.TrimSpace(strings.Trim(strings.TrimSpace(text), ".!")))
	switch t {
	case "STOP", "STOP ALL", "UNSUBSCRIBE", "STOP PROMOTIONS", "OPT OUT", "OPTOUT", "ACHA", "SITISHA":
		return true
	}
	return false
}
