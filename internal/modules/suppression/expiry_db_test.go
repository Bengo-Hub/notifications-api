package suppression

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	entsuppression "github.com/bengobox/notifications-api/internal/ent/suppression"
	"github.com/bengobox/notifications-api/internal/testdb"
)

// A number Meta could not reach (131026) is suppressed for a week, then lapses; a later 131026
// pushes the week out; a permanent suppression is never shortened by one. Runs on the local
// Postgres (testdb).
func TestAddUntilLapsesAndNeverShortensPermanent(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db.Client, []byte("0123456789abcdef0123456789abcdef"))
	ctx := context.Background()
	tid := uuid.New()
	h, other := "hash-unreachable", "hash-bounced"

	if err := s.AddUntil(ctx, &tid, "whatsapp", h, ReasonUnreachable, "whatsapp_status", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Suppressed(ctx, &tid, "whatsapp", []string{h}, false)
	if err != nil || !got[h] {
		t.Fatalf("live suppression not found: %v %v", got, err)
	}
	// Another sender is not affected.
	someone := uuid.New()
	if got, _ := s.Suppressed(ctx, &someone, "whatsapp", []string{h}, false); got[h] {
		t.Fatal("suppression leaked to another sender")
	}

	// Lapsed: moved into the past, it no longer suppresses.
	if _, err := db.Client.Suppression.Update().Where(entsuppression.AddressHash(h)).
		SetExpiresAt(time.Now().Add(-time.Minute)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Suppressed(ctx, &tid, "whatsapp", []string{h}, false); got[h] {
		t.Fatal("a lapsed suppression still suppresses")
	}

	// A later 131026 pushes the same row out again instead of failing on the unique index.
	if err := s.AddUntil(ctx, &tid, "whatsapp", h, ReasonUnreachable, "whatsapp_status", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Suppressed(ctx, &tid, "whatsapp", []string{h}, false); !got[h] {
		t.Fatal("renewed suppression not found")
	}
	if n, _ := db.Client.Suppression.Query().Where(entsuppression.AddressHash(h)).Count(ctx); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}

	// A permanent suppression (hard bounce) stays permanent.
	if err := s.Add(ctx, &tid, "whatsapp", other, ScopeAll, "hard_bounce", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUntil(ctx, &tid, "whatsapp", other, ReasonUnreachable, "whatsapp_status", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	row, err := db.Client.Suppression.Query().Where(entsuppression.AddressHash(other)).Only(ctx)
	if err != nil || row.ExpiresAt != nil || row.Reason != "hard_bounce" {
		t.Fatalf("permanent row changed: %+v %v", row, err)
	}

	// Platform sender (no tenant).
	if err := s.AddUntil(ctx, nil, "whatsapp", h, ReasonUnreachable, "whatsapp_status", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Suppressed(ctx, nil, "whatsapp", []string{h}, false); !got[h] {
		t.Fatal("platform suppression not found")
	}
}
