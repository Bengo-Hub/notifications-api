package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware/pii"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
	"github.com/bengobox/notifications-api/internal/moneyfmt"
)

// Hadia owner gift digest (platform task N3, hadia-api docs/platform-tasks.md "N3 owner digest").
//
// An owner who chose one message a day gets hadia.gift.received with owner_digest "daily". The guest
// receipt goes out as usual; the owner alert is held in digest_items instead (one row per event, so
// a redelivery adds nothing). The scheduled notifier runs the digest job every few minutes; from
// 18:00 Africa/Nairobi (the tenant's digest_hour) it sends each owner one email and one WhatsApp
// with the day's count, total and one line per registry, and a button to the gift log. An owner with
// no held gifts gets nothing.
//
// Once a day per owner: the digest_sends row (tenant, kind, owner key, date) is claimed before
// anything is sent, so a second pod or a restart never sends the day twice. Each queued channel is
// written on the row as it goes, so a pod that dies halfway is picked up after the claim's lease and
// only the missing channel is sent. A gift that arrives after the day's summary went out waits for
// the next day. Guest names and messages never reach the summary.

const (
	digestKindHadiaGift = "hadia_gift"
	// defaultDigestHour is 18:00 local time; a tenant changes it with digest_hour in its auth-api metadata.
	defaultDigestHour = 18
	// digestLease is how long a claimed summary belongs to the pod that claimed it before another
	// run may resume it.
	digestLease = 10 * time.Minute
	// digestKeep is how long a sent summary and its items are kept (they hold contact details).
	digestKeep = 7 * 24 * time.Hour
	// digestPage is how many owners one query lists.
	digestPage = 200
	// digestEvery is how often the scheduled notifier runs the job.
	digestEvery = 5 * time.Minute

	hadiaDigestTemplate   = "hadia/owner_gift_digest"
	hadiaDigestWATemplate = "hadia_owner_gift_digest_v1_btn"
)

// digestTZ is the timezone of the Hadia day (East Africa Time, no daylight saving).
var digestTZ = loadDigestTZ()

func loadDigestTZ() *time.Location {
	if loc, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return loc
	}
	return time.FixedZone("EAT", 3*3600)
}

// digestItem is one held message.
type digestItem struct {
	TenantID     uuid.UUID
	Kind         string
	RecipientKey string
	LocalDate    string // YYYY-MM-DD of the summary it belongs to
	GroupKey     string // registry id
	EventID      string
	AmountMinor  int64
	Data         map[string]any
	CreatedAt    time.Time
}

// digestClaim is a summary this run owns.
type digestClaim struct {
	SendID   uuid.UUID
	Channels []string // already queued by an earlier attempt
}

// digestStalled is a summary a pod claimed and did not finish.
type digestStalled struct {
	TenantID     uuid.UUID
	RecipientKey string
	LocalDate    string
}

// digestStore keeps held items and the once-a-day markers (pgDigestStore in production).
type digestStore interface {
	// Add holds an item; false when its event was already held.
	Add(ctx context.Context, it digestItem) (bool, error)
	// Tenants lists the tenants with items waiting.
	Tenants(ctx context.Context, kind string) ([]uuid.UUID, error)
	// Recipients lists, in key order after the cursor, the people with items waiting for a
	// summary up to the date.
	Recipients(ctx context.Context, kind string, tenantID uuid.UUID, upTo, after string, limit int) ([]string, error)
	// Claim takes the summary for (tenant, kind, recipient, date): a new marker, or one still
	// sending whose claim is older than staleBefore. ok is false when it was sent or another run
	// holds it.
	Claim(ctx context.Context, kind string, tenantID uuid.UUID, recipient, date string, now, staleBefore time.Time) (digestClaim, bool, error)
	// Stalled lists summaries still sending whose claim is older than staleBefore: the pod that took
	// them stopped, and their items are no longer waiting, so only this list brings them back.
	Stalled(ctx context.Context, kind string, staleBefore time.Time, limit int) ([]digestStalled, error)
	// Items returns the items the summary holds; with attach it first gives the summary every item
	// still waiting up to the date.
	Items(ctx context.Context, sendID uuid.UUID, kind string, tenantID uuid.UUID, recipient, upTo string, attach bool) ([]digestItem, error)
	// MarkChannel records that a channel was queued (or left out for an opt-out).
	MarkChannel(ctx context.Context, sendID uuid.UUID, channel string) error
	// Finish marks the summary sent.
	Finish(ctx context.Context, sendID uuid.UUID, items int, now time.Time) error
	// Purge removes sent summaries and their items older than before.
	Purge(ctx context.Context, before time.Time) error
}

// hadiaDigest holds owner alerts and sends the daily summaries.
type hadiaDigest struct {
	store digestStore
	// resolve gives the tenant's contact country and digest hour (tenantResolver.resolve).
	resolve func(ctx context.Context, tenantID string) (*tenantInfo, error)
	// publish queues one notice (sendToParties).
	publish func(ctx context.Context, n partyNotice) error
	// suppressed reports an address that opted out of everything from this tenant.
	suppressed func(ctx context.Context, tenantID uuid.UUID, channel, address string) (bool, error)
	now        func() time.Time
	log        *zap.Logger
}

// newHadiaDigest wires the job to the database, the event bus and the suppression list.
func newHadiaDigest(pool *pgxpool.Pool, tr *tenantResolver, nc *nats.Conn, cfg *config.Config, supp *suppression.Service, logg *zap.Logger) *hadiaDigest {
	d := &hadiaDigest{
		store:   &pgDigestStore{pool: pool},
		resolve: tr.resolve,
		publish: func(ctx context.Context, n partyNotice) error { return sendToParties(ctx, nc, cfg, n) },
		now:     time.Now,
		log:     logg.Named("hadia-digest"),
	}
	if supp != nil {
		d.suppressed = func(ctx context.Context, tenantID uuid.UUID, channel, address string) (bool, error) {
			h := pii.HashAddress(address)
			// Scope "all" only: the digest is transactional, so a marketing opt-out does not stop it.
			got, err := supp.Suppressed(ctx, &tenantID, channel, []string{h}, false)
			return got[h], err
		}
	}
	return d
}

// heldForDigest reports whether a message waits for the owner's daily summary instead of going out.
func heldForDigest(msg hadiaMessage, p map[string]any, d *hadiaDigest) bool {
	return msg.Digest && d != nil && mStr(p, "owner_digest") == "daily"
}

// digestHourOf is the tenant's summary hour (1 to 23), 18 when unset.
func digestHourOf(ti *tenantInfo) int {
	if ti != nil && ti.DigestHour >= 1 && ti.DigestHour <= 23 {
		return ti.DigestHour
	}
	return defaultDigestHour
}

// digestDate is the summary a gift received at t belongs to: that local day's, or the next day's
// when it came at or after the summary hour.
func digestDate(t time.Time, hour int) string {
	l := t.In(digestTZ)
	if l.Hour() >= hour {
		l = l.AddDate(0, 0, 1)
	}
	return l.Format("2006-01-02")
}

// digestDue is the latest summary date due at now: today from the summary hour, else yesterday.
func digestDue(now time.Time, hour int) string {
	l := now.In(digestTZ)
	if l.Hour() < hour {
		l = l.AddDate(0, 0, -1)
	}
	return l.Format("2006-01-02")
}

// hadiaOwnerKey names the owner without their address: the owner id when hadia-api sends it, else
// the contact details. A changed email therefore never splits an owner's day in two.
func hadiaOwnerKey(p map[string]any) string {
	src := "owner:" + mStr(p, "owner_id")
	if mStr(p, "owner_id") == "" {
		src = "contact:" + strings.ToLower(mStr(p, "owner_email")) + "|" + mStr(p, "owner_phone")
	}
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:16])
}

// minorUnits turns "5000.50" into 500050.
func minorUnits(s string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(math.Round(f * 100))
}

// hold keeps a daily owner's gift alert for the summary. The tenant is the event's.
func (d *hadiaDigest) hold(ctx context.Context, ti *tenantInfo, evt eventslib.Event) error {
	p := evt.Payload
	at := evt.Timestamp
	if t, err := time.Parse(time.RFC3339Nano, mStr(p, "received_at")); err == nil {
		at = t
	}
	if at.IsZero() {
		at = d.now()
	}
	currency := mStr(p, "currency")
	if currency == "" {
		currency = "KES"
	}
	_, err := d.store.Add(ctx, digestItem{
		TenantID: evt.TenantID, Kind: digestKindHadiaGift, RecipientKey: hadiaOwnerKey(p),
		LocalDate: digestDate(at, digestHourOf(ti)), GroupKey: mStr(p, "registry_id"), EventID: evt.ID.String(),
		AmountMinor: minorUnits(mStr(p, "amount")), CreatedAt: d.now(),
		// Only what the summary shows: never the guest's name, phone or message.
		Data: map[string]any{"owner_name": mStr(p, "owner_name"), "owner_email": mStr(p, "owner_email"),
			"owner_phone": mStr(p, "owner_phone"), "registry_title": mStr(p, "registry_title"),
			"owner_url": mStr(p, "owner_url"), "currency": currency},
	})
	return err
}

// Run sends every summary that is due. Safe on every pod at once and after a crash: summaries a
// stopped pod left half sent are finished first, then the day's new ones go out.
func (d *hadiaDigest) Run(ctx context.Context) {
	d.resume(ctx)
	tenants, err := d.store.Tenants(ctx, digestKindHadiaGift)
	if err != nil {
		d.log.Warn("digest: tenants not listed", zap.Error(err))
		return
	}
	for _, tenantID := range tenants {
		ti, err := d.resolve(ctx, tenantID.String())
		if err != nil {
			d.log.Warn("digest: tenant not resolved, will retry", zap.String("tenant_id", tenantID.String()), zap.Error(err))
			continue
		}
		upTo := digestDue(d.now(), digestHourOf(ti))
		after := ""
		for {
			recipients, err := d.store.Recipients(ctx, digestKindHadiaGift, tenantID, upTo, after, digestPage)
			if err != nil {
				d.log.Warn("digest: owners not listed", zap.Error(err))
				break
			}
			for _, r := range recipients {
				if err := d.sendOne(ctx, ti, tenantID, r, upTo); err != nil {
					// The claim stays: the next run after the lease resumes the missing channels.
					d.log.Warn("digest: summary not finished, will resume", zap.String("tenant_id", tenantID.String()), zap.Error(err))
				}
			}
			if len(recipients) < digestPage {
				break
			}
			after = recipients[len(recipients)-1]
		}
	}
	if err := d.store.Purge(ctx, d.now().Add(-digestKeep)); err != nil {
		d.log.Warn("digest: old summaries not removed", zap.Error(err))
	}
}

// resume finishes the summaries whose claim went stale.
func (d *hadiaDigest) resume(ctx context.Context) {
	stalled, err := d.store.Stalled(ctx, digestKindHadiaGift, d.now().Add(-digestLease), digestPage)
	if err != nil {
		d.log.Warn("digest: stalled summaries not listed", zap.Error(err))
		return
	}
	for _, st := range stalled {
		ti, err := d.resolve(ctx, st.TenantID.String())
		if err != nil {
			d.log.Warn("digest: tenant not resolved, will retry", zap.String("tenant_id", st.TenantID.String()), zap.Error(err))
			continue
		}
		if err := d.sendOne(ctx, ti, st.TenantID, st.RecipientKey, st.LocalDate); err != nil {
			d.log.Warn("digest: stalled summary not finished, will retry", zap.String("tenant_id", st.TenantID.String()), zap.Error(err))
		}
	}
}

// sendOne sends one owner's summary for the date, or resumes it.
func (d *hadiaDigest) sendOne(ctx context.Context, ti *tenantInfo, tenantID uuid.UUID, recipient, date string) error {
	now := d.now()
	claim, ok, err := d.store.Claim(ctx, digestKindHadiaGift, tenantID, recipient, date, now, now.Add(-digestLease))
	if err != nil || !ok {
		return err
	}
	// A resumed summary keeps the items it started with, so every channel says the same.
	items, err := d.store.Items(ctx, claim.SendID, digestKindHadiaGift, tenantID, recipient, date, len(claim.Channels) == 0)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return d.store.Finish(ctx, claim.SendID, 0, now)
	}
	s := summariseHadiaGifts(items, date)
	data := s.data()
	for _, ch := range defaultPartyChannels {
		if digestHas(claim.Channels, ch) {
			continue
		}
		addr := s.Email
		if ch == "whatsapp" {
			addr = s.Phone
		}
		if addr == "" {
			continue
		}
		if d.suppressed != nil {
			off, err := d.suppressed(ctx, tenantID, ch, addr)
			if err != nil {
				return err
			}
			if off {
				if err := d.store.MarkChannel(ctx, claim.SendID, ch); err != nil {
					return err
				}
				continue
			}
		}
		party := noticeParty{Target: messaging.TargetCustomer, Email: s.Email, Phone: s.Phone, Channels: []string{ch},
			WhatsApp: &partyWhatsApp{Name: hadiaDigestWATemplate, Params: mParams(data, "name", "day", "gifts", "total", "where"), Button: s.button()}}
		if err := d.publish(ctx, partyNotice{
			ServiceID:  "hadia",
			TenantID:   tenantID.String(),
			TemplateID: hadiaDigestTemplate,
			Subject:    "Your Hadia gifts for " + s.Day,
			Scope:      messaging.SenderScopeTenant,
			Data:       data,
			Country:    ti.Country,
			IdemBase:   fmt.Sprintf("digest:%s:%s:%s", tenantID, recipient, date),
			Parties:    []noticeParty{party},
		}); err != nil {
			return err
		}
		if err := d.store.MarkChannel(ctx, claim.SendID, ch); err != nil {
			return err
		}
	}
	return d.store.Finish(ctx, claim.SendID, len(items), now)
}

func digestHas(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// giftDigestLine is one registry in a summary.
type giftDigestLine struct {
	Key, Title, Path string
	Gifts            int
	TotalMinor       int64
}

// giftDigest is one owner's day.
type giftDigest struct {
	Name, Email, Phone, Currency, Day string
	Gifts                             int
	TotalMinor                        int64
	Lines                             []giftDigestLine
}

// summariseHadiaGifts adds up the held gifts. Contact details and titles come from the newest item,
// so a name or number changed during the day is the one used.
func summariseHadiaGifts(items []digestItem, date string) giftDigest {
	sorted := append([]digestItem(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })
	s := giftDigest{Currency: "KES", Day: date}
	if t, err := time.Parse("2006-01-02", date); err == nil {
		s.Day = t.Format("2 Jan 2006")
	}
	lines := map[string]*giftDigestLine{}
	for _, it := range sorted {
		str := func(k string) string { return mStr(it.Data, k) }
		if v := str("owner_name"); v != "" {
			s.Name = v
		}
		if v := str("owner_email"); v != "" {
			s.Email = v
		}
		if v := str("owner_phone"); v != "" {
			s.Phone = v
		}
		if v := str("currency"); v != "" {
			s.Currency = v
		}
		s.Gifts++
		s.TotalMinor += it.AmountMinor
		l, ok := lines[it.GroupKey]
		if !ok {
			l = &giftDigestLine{Key: it.GroupKey}
			lines[it.GroupKey] = l
		}
		l.Gifts++
		l.TotalMinor += it.AmountMinor
		if v := str("registry_title"); v != "" {
			l.Title = v
		}
		if v := hadiaPath(str("owner_url")); v != "" {
			l.Path = v
		}
	}
	for _, l := range lines {
		if l.Title == "" {
			l.Title = "your registry"
		}
		s.Lines = append(s.Lines, *l)
	}
	sort.Slice(s.Lines, func(i, j int) bool {
		if s.Lines[i].Title != s.Lines[j].Title {
			return s.Lines[i].Title < s.Lines[j].Title
		}
		return s.Lines[i].Key < s.Lines[j].Key
	})
	return s
}

func (s giftDigest) money(minor int64) string {
	return moneyfmt.Format(float64(minor)/100, s.Currency)
}

func giftsLabel(n int) string {
	if n == 1 {
		return "1 gift"
	}
	return strconv.Itoa(n) + " gifts"
}

// button is the gift log of the one registry, or the owner's dashboard when there are several.
func (s giftDigest) button() string {
	if len(s.Lines) == 1 && s.Lines[0].Path != "" {
		return s.Lines[0].Path
	}
	return "app"
}

// data is the template data shared by the email and WhatsApp messages.
func (s giftDigest) data() map[string]any {
	where := fmt.Sprintf("%d registries", len(s.Lines))
	if len(s.Lines) == 1 {
		where = s.Lines[0].Title
	}
	regs := make([]map[string]any, 0, len(s.Lines))
	for _, l := range s.Lines {
		r := map[string]any{"title": l.Title, "gifts": giftsLabel(l.Gifts), "total": s.money(l.TotalMinor)}
		if l.Path != "" {
			r["link"] = hadiaButtonBase + "/" + l.Path
		}
		regs = append(regs, r)
	}
	return map[string]any{"name": waParam(s.Name, "there"), "day": s.Day, "gifts": giftsLabel(s.Gifts),
		"total": s.money(s.TotalMinor), "where": waParam(where, "your registry"), "registries": regs,
		"action_link": hadiaButtonBase + "/" + s.button(), "brand_note": hadiaTrustNote}
}

// pgDigestStore keeps the digest in Postgres (digest_items and digest_sends, Ent schemas DigestItem
// and DigestSend). Single statements, so it is safe through the transaction pooler.
type pgDigestStore struct {
	pool *pgxpool.Pool
}

func (s *pgDigestStore) Add(ctx context.Context, it digestItem) (bool, error) {
	data, err := json.Marshal(it.Data)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO digest_items (id, tenant_id, kind, recipient_key, local_date, group_key, event_id, amount_minor, data, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)
		ON CONFLICT (tenant_id, kind, event_id) DO NOTHING`,
		uuid.New(), it.TenantID, it.Kind, it.RecipientKey, it.LocalDate, it.GroupKey, it.EventID, it.AmountMinor, string(data), it.CreatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *pgDigestStore) Tenants(ctx context.Context, kind string) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT tenant_id FROM digest_items WHERE kind = $1 AND send_id IS NULL`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *pgDigestStore) Recipients(ctx context.Context, kind string, tenantID uuid.UUID, upTo, after string, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT recipient_key FROM digest_items
		WHERE kind = $1 AND tenant_id = $2 AND send_id IS NULL AND local_date <= $3 AND recipient_key > $4
		ORDER BY recipient_key LIMIT $5`, kind, tenantID, upTo, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *pgDigestStore) Claim(ctx context.Context, kind string, tenantID uuid.UUID, recipient, date string, now, staleBefore time.Time) (digestClaim, bool, error) {
	// A new marker is ours; an existing one only when it is still sending and its claim went stale
	// (the pod that took it died). A sent marker never comes back.
	rows, err := s.pool.Query(ctx, `
		INSERT INTO digest_sends (id, tenant_id, kind, recipient_key, local_date, status, channels, item_count, claimed_at, created_at)
		VALUES ($1, $2, $3, $4, $5, 'sending', '[]'::jsonb, 0, $6, $6)
		ON CONFLICT (tenant_id, kind, recipient_key, local_date) DO UPDATE SET claimed_at = EXCLUDED.claimed_at
		WHERE digest_sends.status = 'sending' AND digest_sends.claimed_at < $7
		RETURNING id, coalesce(channels, '[]'::jsonb)`,
		uuid.New(), tenantID, kind, recipient, date, now, staleBefore)
	if err != nil {
		return digestClaim{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return digestClaim{}, false, rows.Err()
	}
	var c digestClaim
	var raw []byte
	if err := rows.Scan(&c.SendID, &raw); err != nil {
		return digestClaim{}, false, err
	}
	_ = json.Unmarshal(raw, &c.Channels)
	return c, true, rows.Err()
}

func (s *pgDigestStore) Stalled(ctx context.Context, kind string, staleBefore time.Time, limit int) ([]digestStalled, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, recipient_key, local_date FROM digest_sends
		WHERE kind = $1 AND status = 'sending' AND claimed_at < $2
		ORDER BY claimed_at LIMIT $3`, kind, staleBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []digestStalled
	for rows.Next() {
		var st digestStalled
		if err := rows.Scan(&st.TenantID, &st.RecipientKey, &st.LocalDate); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *pgDigestStore) Items(ctx context.Context, sendID uuid.UUID, kind string, tenantID uuid.UUID, recipient, upTo string, attach bool) ([]digestItem, error) {
	if attach {
		if _, err := s.pool.Exec(ctx, `
			UPDATE digest_items SET send_id = $1
			WHERE kind = $2 AND tenant_id = $3 AND recipient_key = $4 AND local_date <= $5 AND send_id IS NULL`,
			sendID, kind, tenantID, recipient, upTo); err != nil {
			return nil, err
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, kind, recipient_key, local_date, group_key, event_id, amount_minor, coalesce(data, '{}'::jsonb), created_at
		FROM digest_items WHERE send_id = $1 ORDER BY created_at`, sendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []digestItem
	for rows.Next() {
		var it digestItem
		var raw []byte
		if err := rows.Scan(&it.TenantID, &it.Kind, &it.RecipientKey, &it.LocalDate, &it.GroupKey, &it.EventID, &it.AmountMinor, &raw, &it.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &it.Data)
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *pgDigestStore) MarkChannel(ctx context.Context, sendID uuid.UUID, channel string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE digest_sends SET channels = coalesce(channels, '[]'::jsonb) || to_jsonb($2::text)
		WHERE id = $1 AND NOT coalesce(channels, '[]'::jsonb) ? $2`, sendID, channel)
	return err
}

func (s *pgDigestStore) Finish(ctx context.Context, sendID uuid.UUID, items int, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE digest_sends SET status = 'sent', item_count = $2, sent_at = $3 WHERE id = $1`, sendID, items, now)
	return err
}

func (s *pgDigestStore) Purge(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM digest_items WHERE send_id IN (SELECT id FROM digest_sends WHERE status = 'sent' AND sent_at < $1)`, before); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM digest_sends WHERE status = 'sent' AND sent_at < $1`, before)
	return err
}
