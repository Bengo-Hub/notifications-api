// Package contacts finds who a notification goes to. The MarketFlow CRM owns customer
// identity, so when an event carries a CRM contact id the name, email and phone come from the
// CRM at send time. The event's own fields are only a fallback while producers still send
// them, and they cover any CRM outage: a lookup failure never drops a notification.
//
// Nothing here logs personal data. Logs carry tenant and contact ids only.
package contacts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// DefaultTTL is how long a CRM answer is reused before it is fetched again.
	DefaultTTL = 5 * time.Minute
	// DefaultMaxEntries bounds the cache so a burst of events cannot grow it without limit.
	DefaultMaxEntries = 10000
)

// Recipient is the name, email and phone a notification is sent to.
type Recipient struct {
	Name  string
	Email string
	Phone string
}

// lookup is the part of the CRM client the resolver uses, so tests can count calls.
type lookup interface {
	BatchGet(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]crm.Contact, error)
}

type cacheKey struct {
	tenant  uuid.UUID
	contact uuid.UUID
}

type cacheEntry struct {
	rec     Recipient // empty when the CRM does not know the contact
	expires time.Time
}

// Resolver loads CRM contacts through a small in-process cache. It is safe for concurrent use,
// and a nil *Resolver simply returns the fallback.
type Resolver struct {
	crm        lookup
	log        *zap.Logger
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
}

// New builds a resolver over the shared CRM client. A disabled client (no URL or key) is fine:
// every Resolve then returns the fallback.
func New(client *crm.Client, log *zap.Logger) *Resolver {
	return newResolver(client, log, DefaultTTL, DefaultMaxEntries)
}

func newResolver(l lookup, log *zap.Logger, ttl time.Duration, maxEntries int) *Resolver {
	if log == nil {
		log = zap.NewNop()
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Resolver{
		crm:        l,
		log:        log,
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		entries:    make(map[cacheKey]cacheEntry),
	}
}

// Resolve returns the recipient for a CRM contact. Each field comes from the CRM when it has a
// value there, else from fallback. With no tenant or contact id, or when the CRM cannot be
// reached, fallback is returned unchanged.
func (r *Resolver) Resolve(ctx context.Context, tenantID, contactID uuid.UUID, fallback Recipient) Recipient {
	if r == nil || r.crm == nil || tenantID == uuid.Nil || contactID == uuid.Nil {
		return fallback
	}
	fromCRM, ok := r.load(ctx, tenantID, contactID)
	if !ok {
		return fallback
	}
	return Recipient{
		Name:  firstNonEmpty(fromCRM.Name, fallback.Name),
		Email: firstNonEmpty(fromCRM.Email, fallback.Email),
		Phone: firstNonEmpty(fromCRM.Phone, fallback.Phone),
	}
}

// ParseID reads a contact or tenant id from an event field. Blank or malformed values give
// uuid.Nil, which Resolve treats as "no CRM contact".
func ParseID(raw string) uuid.UUID {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil
	}
	return id
}

// load returns the cached or freshly fetched contact. ok is false only when the CRM could not
// answer; a contact the CRM does not know is cached as an empty recipient.
func (r *Resolver) load(ctx context.Context, tenantID, contactID uuid.UUID) (Recipient, bool) {
	key := cacheKey{tenant: tenantID, contact: contactID}
	now := r.now()

	r.mu.Lock()
	if e, hit := r.entries[key]; hit {
		if now.Before(e.expires) {
			r.mu.Unlock()
			return e.rec, true
		}
		delete(r.entries, key)
	}
	r.mu.Unlock()

	found, err := r.crm.BatchGet(ctx, tenantID, []uuid.UUID{contactID})
	if err != nil {
		if !errors.Is(err, crm.ErrDisabled) {
			r.log.Warn("crm contact lookup failed, using event fields",
				zap.String("tenant_id", tenantID.String()),
				zap.String("crm_contact_id", contactID.String()),
				zap.Error(err))
		}
		return Recipient{}, false
	}

	var rec Recipient
	if c, known := found[contactID]; known {
		rec = Recipient{
			Name:  strings.TrimSpace(strings.TrimSpace(c.FirstName) + " " + strings.TrimSpace(c.LastName)),
			Email: strings.TrimSpace(c.Email),
			Phone: strings.TrimSpace(c.Phone),
		}
	} else {
		r.log.Debug("crm contact not found, using event fields",
			zap.String("tenant_id", tenantID.String()),
			zap.String("crm_contact_id", contactID.String()))
	}

	r.mu.Lock()
	r.store(key, cacheEntry{rec: rec, expires: now.Add(r.ttl)}, now)
	r.mu.Unlock()
	return rec, true
}

// store adds an entry, making room first when the cache is full: expired entries go first,
// then any one entry. Callers hold r.mu.
func (r *Resolver) store(key cacheKey, e cacheEntry, now time.Time) {
	if _, exists := r.entries[key]; !exists && len(r.entries) >= r.maxEntries {
		for k, old := range r.entries {
			if !now.Before(old.expires) {
				delete(r.entries, k)
			}
		}
		for k := range r.entries {
			if len(r.entries) < r.maxEntries {
				break
			}
			delete(r.entries, k)
		}
	}
	r.entries[key] = e
}

func firstNonEmpty(a, b string) string {
	if a = strings.TrimSpace(a); a != "" {
		return a
	}
	return strings.TrimSpace(b)
}
