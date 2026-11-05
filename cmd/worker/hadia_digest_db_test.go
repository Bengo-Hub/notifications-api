package main

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/notifications-api/internal/ent"
)

// digestStoreDB gives pgDigestStore a throwaway schema on the local Postgres, so the real statements
// run (the once-a-day marker, the lease, the jsonb channel list). Skipped when it is unavailable.
func digestStoreDB(t *testing.T) *pgDigestStore {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	url := "postgres://postgres:postgres@localhost:5432/notifications?sslmode=disable"
	admin, err := sql.Open("pgx", url)
	if err != nil || admin.Ping() != nil {
		t.Skip("local postgres unavailable")
	}
	schema := "digest_it_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", url+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_ = client.Close()
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	return &pgDigestStore{pool: pool}
}

func TestPgDigestStoreOnceADayLeaseAndChannels(t *testing.T) {
	store := digestStoreDB(t)
	ctx := context.Background()
	tenant := uuid.New()
	const kind, owner, day = "hadia_owner_gift", "owner-a", "2026-11-06"
	now := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	stale := now.Add(-10 * time.Minute)

	item := digestItem{TenantID: tenant, Kind: kind, RecipientKey: owner, LocalDate: day, GroupKey: "reg-1",
		EventID: "evt-1", AmountMinor: 150000, Data: map[string]any{"title": "Wedding"}, CreatedAt: now}
	if added, err := store.Add(ctx, item); err != nil || !added {
		t.Fatalf("first Add = %v, %v", added, err)
	}
	if added, err := store.Add(ctx, item); err != nil || added {
		t.Fatalf("a redelivered event must add nothing: %v, %v", added, err)
	}

	tenants, err := store.Tenants(ctx, kind)
	if err != nil || len(tenants) != 1 || tenants[0] != tenant {
		t.Fatalf("Tenants = %v, %v", tenants, err)
	}
	people, err := store.Recipients(ctx, kind, tenant, day, "", 10)
	if err != nil || len(people) != 1 || people[0] != owner {
		t.Fatalf("Recipients = %v, %v", people, err)
	}
	if before, err := store.Recipients(ctx, kind, tenant, "2026-11-05", "", 10); err != nil || len(before) != 0 {
		t.Fatalf("an item for a later date must not be due: %v, %v", before, err)
	}

	claim, ok, err := store.Claim(ctx, kind, tenant, owner, day, now, stale)
	if err != nil || !ok {
		t.Fatalf("first Claim = %v, %v", ok, err)
	}
	if _, ok, err := store.Claim(ctx, kind, tenant, owner, day, now, stale); err != nil || ok {
		t.Fatalf("a held claim must not be taken again: %v, %v", ok, err)
	}

	got, err := store.Items(ctx, claim.SendID, kind, tenant, owner, day, true)
	if err != nil || len(got) != 1 || got[0].AmountMinor != 150000 || got[0].Data["title"] != "Wedding" {
		t.Fatalf("Items = %+v, %v", got, err)
	}
	if waiting, err := store.Recipients(ctx, kind, tenant, day, "", 10); err != nil || len(waiting) != 0 {
		t.Fatalf("attached items are no longer waiting: %v, %v", waiting, err)
	}

	// Recording a channel twice keeps one entry, and a crashed pod's successor sees it.
	for range 2 {
		if err := store.MarkChannel(ctx, claim.SendID, "email"); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(15 * time.Minute)
	stalled, err := store.Stalled(ctx, kind, later.Add(-10*time.Minute), 10)
	if err != nil || len(stalled) != 1 || stalled[0].RecipientKey != owner {
		t.Fatalf("Stalled = %+v, %v", stalled, err)
	}
	again, ok, err := store.Claim(ctx, kind, tenant, owner, day, later, later.Add(-10*time.Minute))
	if err != nil || !ok || again.SendID != claim.SendID {
		t.Fatalf("stale Claim = %+v, %v, %v", again, ok, err)
	}
	if len(again.Channels) != 1 || again.Channels[0] != "email" {
		t.Fatalf("Channels = %v, want the one already queued", again.Channels)
	}
	if resumed, err := store.Items(ctx, again.SendID, kind, tenant, owner, day, false); err != nil || len(resumed) != 1 {
		t.Fatalf("a resumed summary keeps its items: %v, %v", resumed, err)
	}

	// Sent is final: no later claim, however old, and nothing stalled.
	if err := store.Finish(ctx, claim.SendID, 1, later); err != nil {
		t.Fatal(err)
	}
	farLater := later.Add(48 * time.Hour)
	if _, ok, err := store.Claim(ctx, kind, tenant, owner, day, farLater, farLater); err != nil || ok {
		t.Fatalf("a sent summary must never be claimed again: %v, %v", ok, err)
	}
	if left, err := store.Stalled(ctx, kind, farLater, 10); err != nil || len(left) != 0 {
		t.Fatalf("Stalled after Finish = %+v, %v", left, err)
	}

	// Purge removes only what is older than the cutoff, items first.
	if err := store.Purge(ctx, later.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if kept, err := store.Items(ctx, claim.SendID, kind, tenant, owner, day, false); err != nil || len(kept) != 1 {
		t.Fatalf("Purge removed a recent summary: %v, %v", kept, err)
	}
	if err := store.Purge(ctx, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if gone, err := store.Items(ctx, claim.SendID, kind, tenant, owner, day, false); err != nil || len(gone) != 0 {
		t.Fatalf("Purge left items: %v, %v", gone, err)
	}
}
