package announcements

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

	"github.com/bengobox/notifications-api/internal/ent"
)

// testClient opens an ent client on a throwaway schema of the local Postgres (skipped when it is
// unavailable or SKIP_POSTGRES_TESTS is set).
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
	schema := "ann_it_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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

// Expired announcements are hard deleted; running and open-ended ones stay, and the public read
// never returns an expired one even before the purge runs.
func TestPurgeExpiredHardDeletes(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	svc := NewService(client)
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)

	mk := func(title string, ends *time.Time) {
		in := Input{Title: title, Summary: "s", Services: []string{"pos"}, StartsAt: ptr(now.Add(-2 * time.Hour)), EndsAt: ends}
		if _, err := svc.Create(ctx, in, "admin@example.com", nil); err != nil {
			t.Fatal(err)
		}
	}
	mk("expired", &past)
	mk("running", &future)
	mk("open-ended", nil)

	active, err := svc.Active(ctx, "pos", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("active = %v, want running and open-ended", titles(active))
	}

	n, err := svc.PurgeExpired(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	left, _ := svc.List(ctx, nil)
	if len(left) != 2 {
		t.Fatalf("left = %v, want running and open-ended", titles(left))
	}
	if n, _ := svc.PurgeExpired(ctx, now); n != 0 {
		t.Fatalf("second purge deleted %d, want 0 (idempotent)", n)
	}
}

func ptr[T any](v T) *T { return &v }
