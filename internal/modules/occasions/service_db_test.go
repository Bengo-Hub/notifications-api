package occasions

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
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/occasion"
)

func testClient(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	url := "postgres://postgres:postgres@localhost:5432/notifications?sslmode=disable"
	admin, err := sql.Open("pgx", url)
	if err != nil || admin.Ping() != nil {
		t.Skip("local postgres unavailable")
	}
	schema := "occ_it_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	db, err := sql.Open("pgx", url+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	return client
}

// Occasions still naming a template Meta refused move to its replacement on the next seed; any
// other name (a sender's own choice) is left alone.
func TestSeedUpgradesSupersededTemplates(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	svc := NewService(client, zap.NewNop())
	if err := svc.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	set := func(key, tpl string) {
		o, err := client.Occasion.Query().Where(occasion.TenantIDIsNil(), occasion.Key(key)).Only(ctx)
		if err != nil {
			t.Fatal(err)
		}
		meta := o.Metadata
		meta["whatsapp_template"] = tpl
		if err := client.Occasion.UpdateOneID(o.ID).SetMetadata(meta).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	get := func(key string) string {
		o, _ := client.Occasion.Query().Where(occasion.TenantIDIsNil(), occasion.Key(key)).Only(ctx)
		s, _ := o.Metadata["whatsapp_template"].(string)
		return s
	}
	set("customer_service_week", "occasion_customer_service_week_v1")
	set("easter", "my_own_easter_template")
	if err := svc.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := get("customer_service_week"); got != "occasion_customer_service_week_v2" {
		t.Errorf("superseded name not upgraded: %s", got)
	}
	if got := get("easter"); got != "my_own_easter_template" {
		t.Errorf("a sender's own template must not change: %s", got)
	}
}

// A tenant's customised catalogue occasion must carry the catalogue id everywhere (List, Get and
// the planner's DueSoon): the one-draft-per-occasion-year guard keys on it. On 2026-10-07 the
// planner used the tenant row's id while "Prepare this year's" used the catalogue id, and a
// tenant ended up with two Mazingira Day drafts.
func TestCustomisedOccasionKeepsCatalogueID(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	svc := NewService(client, zap.NewNop())
	if err := svc.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	if err := svc.Save(ctx, &tenant, "mazingira_day", Update{Settings: &Settings{Enabled: true, Channels: []string{"email"}}}); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Africa/Nairobi")
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, loc)
	listed, err := svc.Get(ctx, &tenant, "mazingira_day", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	due, err := svc.DueSoon(ctx, now, loc)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		if d.TenantID != nil && *d.TenantID == tenant && d.View.Key == "mazingira_day" {
			found = true
			if d.View.ID != listed.ID {
				t.Fatalf("planner id %s differs from the listed id %s", d.View.ID, listed.ID)
			}
		}
	}
	if !found {
		t.Fatal("the enabled tenant occasion should be due within its lead days")
	}
}
