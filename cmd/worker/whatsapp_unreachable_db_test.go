package main

import (
	"context"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware/pii"
	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/modules/suppression"
	"github.com/bengobox/notifications-api/internal/testdb"
)

// The webhook records 131026 against the number as Meta reports it ("2547..."); the worker must
// find it whatever form the message holds ("+254 7...", "07..."), for that sender only, and only
// until it lapses. The platform number is recorded as null or as the platform id. Runs on the local
// Postgres (testdb).
func TestWhatsAppUnreachable(t *testing.T) {
	db := testdb.Open(t)
	pool := db.Pool(t)
	ctx := context.Background()
	s := suppression.NewService(db.Client, []byte("0123456789abcdef0123456789abcdef"))
	tenant, other, platform := uuid.New(), uuid.New(), uuid.New()
	week := time.Now().Add(7 * 24 * time.Hour)
	if err := s.AddUntil(ctx, &tenant, "whatsapp", pii.HashAddress("254712345678"), suppression.ReasonUnreachable, "whatsapp_status", week); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUntil(ctx, nil, "whatsapp", pii.HashAddress("254799999999"), suppression.ReasonUnreachable, "whatsapp_status", week); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, sender, to string
		want             bool
	}{
		{"international", tenant.String(), "254712345678", true},
		{"spaced plus form", tenant.String(), "+254 712 345 678", true},
		{"local form", tenant.String(), "0712345678", true},
		{"another sender", other.String(), "254712345678", false},
		{"another number", tenant.String(), "254700000001", false},
		{"platform number, null sender row", platform.String(), "0799999999", true},
	}
	for _, c := range cases {
		if got := whatsAppUnreachable(ctx, pool, c.sender, platform.String(), c.to, map[string]any{}); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// A lapsed one no longer blocks.
	if _, err := pool.Exec(ctx, "UPDATE suppressions SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatal(err)
	}
	if whatsAppUnreachable(ctx, pool, tenant.String(), platform.String(), "254712345678", map[string]any{}) {
		t.Fatal("lapsed suppression still blocks")
	}
}
