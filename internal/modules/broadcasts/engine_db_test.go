package broadcasts

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware/pii"

	"github.com/bengobox/notifications-api/internal/ent"
	entbroadcast "github.com/bengobox/notifications-api/internal/ent/broadcast"
	entrecipient "github.com/bengobox/notifications-api/internal/ent/broadcastrecipient"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/occasions"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
)

// testDB opens an ent client and a pgx pool on a throwaway schema of the local Postgres (skipped
// when it is unavailable or SKIP_POSTGRES_TESTS is set).
func testDB(t *testing.T) (*ent.Client, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	url := "postgres://postgres:postgres@localhost:5432/notifications?sslmode=disable"
	admin, err := sql.Open("pgx", url)
	if err != nil || admin.Ping() != nil {
		t.Skip("local postgres unavailable")
	}
	schema := "bc_it_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	return client, pool
}

type fakeResolver struct{ people []Person }

func (f fakeResolver) Page(_ context.Context, _ map[string]any, _ *uuid.UUID, after string, limit int) ([]Person, string, error) {
	start := 0
	for i, p := range f.people {
		if p.Key == after {
			start = i + 1
		}
	}
	end := min(start+limit, len(f.people))
	next := ""
	if end < len(f.people) {
		next = f.people[end-1].Key
	}
	return f.people[start:end], next, nil
}

func platformBroadcast(t *testing.T, client *ent.Client, channels []string) *ent.Broadcast {
	t.Helper()
	b, err := client.Broadcast.Create().SetScope(entbroadcast.ScopePlatform).SetKind(entbroadcast.KindGreeting).
		SetClass(entbroadcast.ClassMarketing).SetStatus(entbroadcast.StatusSending).SetTitle("CSW").
		SetChannels(channels).
		SetContent(contentMap(Content{
			Email:    &EmailContent{Subject: "Happy {occasion}", Body: "Dear {first_name},\n\nThank you.\n\nFrom {sender_name}."},
			SMS:      &SMSContent{Body: "Dear {first_name}, thank you. {sender_name}"},
			WhatsApp: &WhatsAppContent{Template: "occasion_customer_service_week_v2", Params: []string{"first_name", "sender_name"}},
		})).
		SetAudience(map[string]any{"type": AudiencePlatformTenants}).SetSendAt(time.Now()).
		SetMetadata(map[string]any{"sender_name": "Codevertex Africa", "occasion_name": "Customer Service Week", "timezone": "Africa/Nairobi"}).
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMaterialiseChoosesFirstValidAddressAndAppliesOptOuts(t *testing.T) {
	client, pool := testDB(t)
	ctx := context.Background()
	supp := suppression.NewService(client, []byte("0123456789abcdef0123456789abcdef"))
	// Tenant B's owner email is opted out of platform marketing.
	if err := supp.Add(ctx, nil, "email", pii.HashAddress("owner@b.co.ke"), suppression.ScopeMarketing, "unsubscribe", "test"); err != nil {
		t.Fatal(err)
	}
	people := []Person{
		{Key: "a", BusinessName: "Urban Loft", Region: "KE",
			Emails: []Address{{Value: "not-an-email", Source: "owner"}, {Value: "Titus@UrbanLoft.co.ke", Source: "owner", FirstName: "Titus"}},
			Phones: []Address{{Value: "0712345678", Source: "owner", FirstName: "Titus"}, {Value: "+254 712 345 678", Source: "tenant"}, {Value: "0722000111", Source: "main_outlet"}}},
		{Key: "b", BusinessName: "Shop B", Region: "KE",
			Emails: []Address{{Value: "owner@b.co.ke", Source: "owner"}},
			Phones: []Address{{Value: "12345", Source: "tenant"}}},
		{Key: "c", BusinessName: "Kampala Co", Region: "UG",
			Phones: []Address{{Value: "0772123456", Source: "tenant"}}}, // local Ugandan number read with the tenant's country
	}
	e := &Engine{Client: client, Pool: pool, Suppress: supp, Log: zap.NewNop(),
		Resolvers: map[string]Resolver{AudiencePlatformTenants: fakeResolver{people: people}}}
	b := platformBroadcast(t, client, []string{"email", "whatsapp"})
	if err := e.materialise(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := e.materialise(ctx, b); err != nil { // a second run must add nothing
		t.Fatal(err)
	}
	rows, err := client.BroadcastRecipient.Query().Where(entrecipient.BroadcastID(b.ID)).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("want one row per person per channel (6), got %d", len(rows))
	}
	got := map[string]*ent.BroadcastRecipient{}
	for _, r := range rows {
		got[r.DisplayName+"/"+r.Channel] = r
	}
	if r := got["Titus/email"]; r == nil || r.Status != entrecipient.StatusPending || *r.Address != "titus@urbanloft.co.ke" {
		t.Errorf("Urban Loft email: %+v", r)
	}
	wa := got["Titus/whatsapp"]
	if wa == nil || *wa.Address != "+254712345678" {
		t.Fatalf("Urban Loft whatsapp: %+v", wa)
	}
	if backups := stringList(wa.Vars["backups"]); len(backups) != 1 || backups[0] != "+254722000111" {
		t.Errorf("duplicate number dropped, outlet number kept as backup: %v", backups)
	}
	if r := got["Shop B/email"]; r == nil || r.Status != entrecipient.StatusSuppressed || r.Address != nil {
		t.Errorf("opted-out address is suppressed and not stored: %+v", r)
	}
	if r := got["Shop B/whatsapp"]; r == nil || r.Status != entrecipient.StatusSkipped || r.Error != "no valid phone number" {
		t.Errorf("invalid phone is skipped with a reason: %+v", r)
	}
	if r := got["Kampala Co/whatsapp"]; r == nil || *r.Address != "+256772123456" {
		t.Errorf("Ugandan local number: %+v", r)
	}
	fresh, _ := client.Broadcast.Get(ctx, b.ID)
	if fresh.TargetCount != 6 || fresh.SuppressedCount != 1 || fresh.SkippedCount != 2 {
		t.Errorf("counts: target %d suppressed %d skipped %d", fresh.TargetCount, fresh.SuppressedCount, fresh.SkippedCount)
	}
}

func TestReviewAndExclusionsMatchWhatIsSent(t *testing.T) {
	client, pool := testDB(t)
	ctx := context.Background()
	people := []Person{
		{Key: "a", BusinessName: "Urban Loft", Region: "KE", Emails: []Address{{Value: "titus@urbanloft.co.ke", FirstName: "Titus"}}},
		{Key: "b", BusinessName: "Shop B", Region: "KE", Emails: []Address{{Value: "owner@shopb.co.ke"}}},
		{Key: "c", BusinessName: "Shop C", Region: "KE", Phones: []Address{{Value: "12"}}},
	}
	res := fakeResolver{people: people}
	supp := suppression.NewService(client, []byte("0123456789abcdef0123456789abcdef"))
	svc := NewService(client, zap.NewNop())
	b := platformBroadcast(t, client, []string{"email"})
	if err := client.Broadcast.UpdateOneID(b.ID).SetStatus(entbroadcast.StatusPendingApproval).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	rows, next, err := Review(ctx, res, supp, b, "", 50)
	if err != nil || next != "" || len(rows) != 3 {
		t.Fatalf("review: %d rows, next %q, %v", len(rows), next, err)
	}
	if r := rows[0]; r.Name != "Titus" || !r.Channels["email"].Sends || r.Channels["email"].Address != "t***@urbanloft.co.ke" {
		t.Errorf("row a: %+v", r)
	}
	if r := rows[2]; r.Channels["email"].Sends || r.Channels["email"].Reason != "no valid email" {
		t.Errorf("row c has no email and is shown as not sending: %+v", r)
	}

	// The sender unticks Shop B.
	if n, err := svc.SetExclusions(ctx, b.ID, nil, []string{"b"}, nil); err != nil || n != 1 {
		t.Fatalf("exclude: %d %v", n, err)
	}
	b, _ = client.Broadcast.Get(ctx, b.ID)
	rows, _, _ = Review(ctx, res, supp, b, "", 50)
	if !rows[1].Excluded || rows[1].Channels["email"].Sends || rows[1].Channels["email"].Reason != "left out" {
		t.Errorf("Shop B is shown as left out: %+v", rows[1])
	}

	// Sending honours it.
	if err := client.Broadcast.UpdateOneID(b.ID).SetStatus(entbroadcast.StatusSending).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ = client.Broadcast.Get(ctx, b.ID)
	e := &Engine{Client: client, Pool: pool, Suppress: supp, Log: zap.NewNop(), Resolvers: map[string]Resolver{AudiencePlatformTenants: res}}
	if err := e.materialise(ctx, b); err != nil {
		t.Fatal(err)
	}
	pending, _ := client.BroadcastRecipient.Query().Where(entrecipient.BroadcastID(b.ID), entrecipient.StatusEQ(entrecipient.StatusPending)).Count(ctx)
	left, _ := client.BroadcastRecipient.Query().Where(entrecipient.BroadcastID(b.ID), entrecipient.ErrorEQ("left out by the sender")).Count(ctx)
	if pending != 1 || left != 1 {
		t.Errorf("only Urban Loft is sent to (pending %d), Shop B recorded as left out (%d)", pending, left)
	}

	// Once sending, the list is fixed.
	if _, err := svc.SetExclusions(ctx, b.ID, nil, nil, []string{"b"}); err == nil {
		t.Error("exclusions must not change after sending starts")
	}
}

func TestClaimNeverHandsTheSameRowToTwoWorkers(t *testing.T) {
	client, pool := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := platformBroadcast(t, client, []string{"email"})
	builders := make([]*ent.BroadcastRecipientCreate, 0, 120)
	for i := 0; i < 120; i++ {
		addr := uuid.NewString() + "@x.co.ke"
		builders = append(builders, client.BroadcastRecipient.Create().SetBroadcastID(b.ID).SetChannel("email").
			SetAddress(addr).SetAddressHash(pii.HashAddress(addr)))
	}
	if err := client.BroadcastRecipient.CreateBulk(builders...).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Client: client, Pool: pool, Log: zap.NewNop()}
	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				rows, err := e.claim(ctx, b.ID, "email", 7)
				if err != nil {
					t.Error(err)
					return
				}
				if len(rows) == 0 {
					return
				}
				mu.Lock()
				for _, r := range rows {
					seen[r.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 120 {
		t.Fatalf("claimed %d distinct rows, want 120", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("row %s claimed %d times", id, n)
		}
	}
}

func TestRecordOutcomeRetriesTransientAndStopsPermanent(t *testing.T) {
	client, _ := testDB(t)
	ctx := context.Background()
	b := platformBroadcast(t, client, []string{"email"})
	mk := func(addr string) *ent.BroadcastRecipient {
		r, err := client.BroadcastRecipient.Create().SetBroadcastID(b.ID).SetChannel("email").SetAddress(addr).
			SetAddressHash(pii.HashAddress(addr)).SetStatus(entrecipient.StatusDispatching).SetAttempts(1).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	msgFor := func(r *ent.BroadcastRecipient) *messaging.Message {
		return &messaging.Message{Metadata: map[string]any{MetaRecipientID: r.ID.String()}}
	}
	ok, transient, permanentErr := mk("a@x.co.ke"), mk("b@x.co.ke"), mk("c@x.co.ke")
	RecordOutcome(ctx, client, msgFor(ok), "sent", nil)
	RecordOutcome(ctx, client, msgFor(transient), "failed", errors.New("421 try again later"))
	RecordOutcome(ctx, client, msgFor(permanentErr), "failed", errors.New("550 5.1.1 user unknown"))
	RecordOutcome(ctx, client, msgFor(ok), "failed", errors.New("late duplicate")) // ignored: no longer dispatching

	check := func(r *ent.BroadcastRecipient, want entrecipient.Status) *ent.BroadcastRecipient {
		got, _ := client.BroadcastRecipient.Get(ctx, r.ID)
		if got.Status != want {
			t.Errorf("%s: status %s, want %s", *got.Address, got.Status, want)
		}
		return got
	}
	check(ok, entrecipient.StatusSent)
	if got := check(transient, entrecipient.StatusPending); got.NextAttemptAt == nil || !got.NextAttemptAt.After(time.Now()) {
		t.Error("a transient failure is retried later")
	}
	check(permanentErr, entrecipient.StatusFailed)
}

func TestBuildMessagePersonalisesAndAddsOptOuts(t *testing.T) {
	supp := suppression.NewService(nil, []byte("0123456789abcdef0123456789abcdef"))
	e := &Engine{PlatformID: uuid.NewString(), PublicURL: "https://notificationsapi.example", Suppress: supp}
	b := &ent.Broadcast{ID: uuid.New(), Class: entbroadcast.ClassMarketing, Metadata: map[string]any{}}
	content := Content{
		Email:    &EmailContent{Subject: "Happy {occasion}", Body: "Dear {first_name},\n\nThank you.\n\nFrom {sender_name}."},
		SMS:      &SMSContent{Body: "Dear {first_name}, thank you."},
		WhatsApp: &WhatsAppContent{Template: "occasion_customer_service_week_v2", Params: []string{"first_name", "sender_name"}},
	}
	vars := Vars{FirstName: "Titus", SenderName: "Codevertex Africa", Occasion: "Customer Service Week"}.Map()

	email, err := e.buildMessage(b, content, claimedRow{ID: uuid.New(), Channel: "email", Address: "titus@x.co.ke", Vars: vars})
	if err != nil {
		t.Fatal(err)
	}
	if email.Metadata["subject"] != "Happy Customer Service Week" || email.SenderScope != messaging.SenderScopePlatform {
		t.Errorf("email: %+v", email.Metadata)
	}
	if p := email.Data["paragraphs"].([]string); len(p) != 3 || p[0] != "Dear Titus," {
		t.Errorf("paragraphs: %v", p)
	}
	u, _ := email.Metadata[MetaUnsubscribe].(string)
	if !strings.HasPrefix(u, "https://notificationsapi.example/api/v1/public/unsubscribe/") {
		t.Errorf("marketing email carries a one-click unsubscribe link: %q", u)
	}

	sms, _ := e.buildMessage(b, content, claimedRow{ID: uuid.New(), Channel: "sms", Address: "+254712345678", Vars: vars})
	if !strings.HasSuffix(sms.Data["body"].(string), "Reply STOP to opt out") {
		t.Errorf("marketing SMS carries the opt-out: %q", sms.Data["body"])
	}

	wa, _ := e.buildMessage(b, content, claimedRow{ID: uuid.New(), Channel: "whatsapp", Address: "+254712345678", Vars: vars})
	params := wa.Metadata["template_params"].([]string)
	if wa.Metadata["template_name"] != "occasion_customer_service_week_v2" || len(params) != 2 || params[0] != "Titus" || params[1] != "Codevertex Africa" {
		t.Errorf("whatsapp template: %+v", wa.Metadata)
	}
}

func TestDraftIsCreatedOncePerOccasionYear(t *testing.T) {
	client, _ := testDB(t)
	ctx := context.Background()
	occ := occasions.NewService(client, zap.NewNop())
	if err := occ.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Africa/Nairobi")
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, loc)
	v, err := occ.Get(ctx, nil, "customer_service_week", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	dr := &Drafter{Client: client, Occasions: occ, Log: zap.NewNop()}
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)
	due := occasions.Due{View: v, Start: start, SendOn: now, Year: 2026}
	b1, created1, err := dr.Draft(ctx, due, loc)
	if err != nil || !created1 {
		t.Fatalf("first draft: %v %v", created1, err)
	}
	b2, created2, err := dr.Draft(ctx, due, loc)
	if err != nil || created2 || b2.ID != b1.ID {
		t.Fatalf("second draft must return the first: %v %v", created2, err)
	}
	if b1.Status != entbroadcast.StatusPendingApproval {
		t.Errorf("generated drafts wait for approval, got %s", b1.Status)
	}
	c := ContentOf(b1)
	if c.Email == nil || c.WhatsApp == nil || c.WhatsApp.Template != "occasion_customer_service_week_v2" {
		t.Errorf("CSW draft channels: %+v", c)
	}
	if typ, _ := b1.Audience["type"].(string); typ != AudiencePlatformTenants {
		t.Errorf("platform occasion goes to tenants: %v", b1.Audience)
	}
}
