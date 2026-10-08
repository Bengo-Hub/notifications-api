package broadcasts

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/httpware/pii"
	ratelimit "github.com/Bengo-Hub/shared-ratelimit"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	entannouncement "github.com/bengobox/notifications-api/internal/ent/announcement"
	entbroadcast "github.com/bengobox/notifications-api/internal/ent/broadcast"
	entrecipient "github.com/bengobox/notifications-api/internal/ent/broadcastrecipient"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/occasions"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
)

// Message metadata keys the worker reads to report a broadcast send's outcome.
const (
	MetaRecipientID = "broadcast_recipient_id"
	MetaBroadcastID = "broadcast_id"
	MetaUnsubscribe = "list_unsubscribe_url"
	// TemplateID is the template every broadcast message renders through.
	TemplateID = "broadcast/message"
)

// Pacing defaults, per sending account. Email stays under the worker's own SMTP guard; SMS and
// WhatsApp stay well under Africa's Talking and Meta Cloud API throughput.
var defaultPace = map[string]ratelimit.Options{
	"email":    {Name: "broadcast-email", Limit: 120, Window: time.Minute, Burst: 20},
	"sms":      {Name: "broadcast-sms", Limit: 10, Window: time.Second, Burst: 10},
	"whatsapp": {Name: "broadcast-whatsapp", Limit: 20, Window: time.Second, Burst: 20},
	"push":     {Name: "broadcast-push", Limit: 50, Window: time.Second, Burst: 50},
}

const (
	pageSize          = 200 // audience page per resolver call
	pagesPerTick      = 10  // materialiser pages per broadcast per tick
	claimBatch        = 50  // most rows claimed per channel per tick
	maxAttempts       = 3   // transient provider failures are retried this many times
	staleDispatching  = 30 * time.Minute
	defaultWindowFrom = 8  // marketing sends start at 08:00 local
	defaultWindowTo   = 20 // and stop at 20:00 local
)

// Engine runs the background work for broadcasts: the occasion planner, the materialiser (turns
// an audience into recipient rows) and the dispatcher (paces recipients into the worker queue).
// It runs in the worker binary on every pod; the jobs are safe to run concurrently.
type Engine struct {
	Client     *ent.Client
	Pool       *pgxpool.Pool
	NATS       *nats.Conn
	Events     config.EventsConfig
	Redis      redis.UniversalClient
	Limiter    *ratelimit.Limiter
	Suppress   *suppression.Service
	Occasions  *occasions.Service
	Resolvers  map[string]Resolver
	PlatformID string // the platform tenant's UUID (platform sends go out on its providers)
	PublicURL  string // this API's public base, for unsubscribe links
	Log        *zap.Logger
}

func (e *Engine) drafter() *Drafter {
	return &Drafter{Client: e.Client, Occasions: e.Occasions, PlatformID: e.PlatformID, Log: e.Log}
}

// upgradeTemplates points broadcasts that have not started sending at the replacement of a
// superseded WhatsApp template (see occasions.ReplacementTemplate). Exact names only; safe to
// run on every start and on every pod.
func (e *Engine) upgradeTemplates(ctx context.Context) {
	open, err := e.Client.Broadcast.Query().
		Where(entbroadcast.StatusIn(entbroadcast.StatusDraft, entbroadcast.StatusPendingApproval,
			entbroadcast.StatusRejected, entbroadcast.StatusScheduled, entbroadcast.StatusPaused)).
		All(ctx)
	if err != nil {
		e.Log.Warn("template upgrade: query failed", zap.Error(err))
		return
	}
	platformName := e.drafter().SenderName(ctx, nil)
	for _, b := range open {
		// Generated platform drafts sign off with the platform's current name (a rename in
		// auth-api reaches them); a name a sender typed is theirs to keep.
		if generated, _ := b.Metadata["generated"].(bool); generated && b.TenantID == nil {
			if cur, _ := b.Metadata["sender_name"].(string); cur != platformName {
				meta := b.Metadata
				meta["sender_name"] = platformName
				if err := e.Client.Broadcast.UpdateOneID(b.ID).SetMetadata(meta).Exec(ctx); err != nil {
					e.Log.Warn("sender name refresh failed", zap.String("broadcast", b.ID.String()), zap.Error(err))
				}
			}
		}
		c := ContentOf(b)
		if c.WhatsApp == nil {
			continue
		}
		next, ok := occasions.ReplacementTemplate(c.WhatsApp.Template)
		if !ok {
			continue
		}
		c.WhatsApp.Template = next
		if err := e.Client.Broadcast.UpdateOneID(b.ID).SetContent(contentMap(c)).Exec(ctx); err != nil {
			e.Log.Warn("template upgrade failed", zap.String("broadcast", b.ID.String()), zap.Error(err))
			continue
		}
		e.Log.Info("broadcast moved to the replacement WhatsApp template", zap.String("broadcast", b.ID.String()), zap.String("template", next))
	}
}

// Start runs the loops until ctx ends.
func (e *Engine) Start(ctx context.Context) {
	e.Log = e.Log.Named("broadcasts")
	e.upgradeTemplates(ctx)
	go e.loop(ctx, "planner", time.Hour, func(ctx context.Context) {
		_, _ = sharedcache.RunOnce(ctx, e.Redis, e.Log, sharedcache.PeriodKey("notifications:broadcasts:planner", time.Hour), 10*time.Minute, e.drafter().Plan)
	})
	go e.loop(ctx, "materialiser", 20*time.Second, func(ctx context.Context) { e.materialiseDue(ctx) })
	go e.loop(ctx, "dispatcher", 2*time.Second, func(ctx context.Context) { e.dispatchTick(ctx) })
	go e.loop(ctx, "reaper", 5*time.Minute, func(ctx context.Context) { e.reapStale(ctx) })
}

func (e *Engine) loop(ctx context.Context, name string, every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					e.Log.Error("broadcast job panicked", zap.String("job", name), zap.Any("panic", r))
				}
			}()
			fn(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ----------------------------------------------------------------------------------------------
// Materialiser: audience pages become recipient rows.

func (e *Engine) materialiseDue(ctx context.Context) {
	now := time.Now()
	due, err := e.Client.Broadcast.Query().
		Where(entbroadcast.StatusIn(entbroadcast.StatusScheduled, entbroadcast.StatusSending), entbroadcast.SendAtLTE(now)).
		Order(ent.Asc(entbroadcast.FieldSendAt)).Limit(20).All(ctx)
	if err != nil {
		e.Log.Warn("materialiser: due query failed", zap.Error(err))
		return
	}
	for _, b := range due {
		if done, _ := b.Metadata["materialised"].(bool); done && b.Status == entbroadcast.StatusSending {
			continue
		}
		// One pod per broadcast at a time; inserts are idempotent anyway.
		lock, ok, err := sharedcache.TryLock(ctx, e.Redis, "notifications:broadcasts:materialise:"+b.ID.String(), 2*time.Minute)
		if err != nil || !ok {
			continue
		}
		if err := e.materialise(ctx, b); err != nil {
			e.Log.Warn("materialise failed", zap.String("broadcast", b.ID.String()), zap.Error(err))
		}
		_ = lock.Release(ctx)
	}
}

func (e *Engine) materialise(ctx context.Context, b *ent.Broadcast) error {
	if b.Status == entbroadcast.StatusScheduled {
		n, err := e.Client.Broadcast.Update().Where(entbroadcast.ID(b.ID), entbroadcast.StatusEQ(entbroadcast.StatusScheduled)).
			SetStatus(entbroadcast.StatusSending).Save(ctx)
		if err != nil || n == 0 {
			return err
		}
		if err := e.publishBanner(ctx, b); err != nil {
			e.Log.Warn("broadcast banner failed", zap.String("broadcast", b.ID.String()), zap.Error(err))
		}
	}
	meta := cloneMap(b.Metadata)
	aType, _ := b.Audience["type"].(string)
	recipientChannels := deliverableChannels(b.Channels)
	if aType == "" || len(recipientChannels) == 0 {
		meta["materialised"] = true
		return e.Client.Broadcast.UpdateOneID(b.ID).SetMetadata(meta).Exec(ctx)
	}
	resolver, ok := e.Resolvers[aType]
	if !ok {
		meta["materialised"] = true
		meta["audience_error"] = ErrAudienceUnavailable.Error()
		return e.Client.Broadcast.UpdateOneID(b.ID).SetMetadata(meta).SetStatus(entbroadcast.StatusFailed).Exec(ctx)
	}
	cursor, _ := meta["cursor"].(string)
	for i := 0; i < pagesPerTick; i++ {
		people, next, err := resolver.Page(ctx, b.Audience, b.TenantID, cursor, pageSize)
		if errors.Is(err, ErrAudienceUnavailable) {
			// Retrying cannot help: stop with the reason the detail view shows.
			meta["audience_error"] = err.Error()
			return e.Client.Broadcast.UpdateOneID(b.ID).SetMetadata(meta).SetStatus(entbroadcast.StatusFailed).
				SetCompletedAt(time.Now()).Exec(ctx)
		}
		if err != nil {
			return err
		}
		if err := e.insertRecipients(ctx, b, people, recipientChannels); err != nil {
			return err
		}
		cursor = next
		meta["cursor"] = cursor
		if next == "" {
			meta["materialised"] = true
			break
		}
	}
	if err := e.Client.Broadcast.UpdateOneID(b.ID).SetMetadata(meta).Exec(ctx); err != nil {
		return err
	}
	return RefreshCounts(ctx, e.Client, b.ID)
}

func deliverableChannels(channels []string) []string {
	var out []string
	for _, c := range channels {
		if c == "email" || c == "sms" || c == "whatsapp" {
			out = append(out, c)
		}
	}
	return out
}

// insertRecipients turns one page of people into recipient rows (selectCandidates decides the
// address per channel; opt-outs and the sender's exclusions applied), one row per address per
// channel. Re-running a page inserts nothing new.
func (e *Engine) insertRecipients(ctx context.Context, b *ent.Broadcast, people []Person, channels []string) error {
	if len(people) == 0 {
		return nil
	}
	senderName, _ := b.Metadata["sender_name"].(string)
	occasionName, _ := b.Metadata["occasion_name"].(string)
	year := time.Now().Year()
	if b.OccasionYear != nil {
		year = *b.OccasionYear
	}
	cands := selectCandidates(b, people, channels)
	if err := markSuppressed(ctx, e.Suppress, b, cands, channels); err != nil {
		return err
	}

	builders := make([]*ent.BroadcastRecipientCreate, 0, len(cands))
	for _, c := range cands {
		vars := Vars{FirstName: c.first, Name: c.person.Name, BusinessName: c.person.BusinessName, SenderName: senderName, Occasion: occasionName, Year: year}
		vm := vars.Map()
		if len(c.backups) > 0 {
			vm["backups"] = c.backups
		}
		r := e.Client.BroadcastRecipient.Create().
			SetBroadcastID(b.ID).SetChannel(c.channel).SetDisplayName(firstNonEmpty(c.first, c.person.BusinessName, c.person.Name)).SetVars(vm)
		if b.TenantID != nil {
			r.SetTenantID(*b.TenantID)
		}
		if c.person.RecipientTenantID != nil {
			r.SetRecipientTenantID(*c.person.RecipientTenantID)
		}
		switch {
		case c.address == "":
			// Keep a row so the detail view shows who could not be reached and why.
			r.SetAddressHash(pii.HashAddress("none:" + c.channel + ":" + c.person.Key)).
				SetStatus(entrecipient.StatusSkipped).SetError(c.reason)
		case c.suppressed:
			r.SetAddressHash(pii.HashAddress(c.address)).SetStatus(entrecipient.StatusSuppressed).SetError("opted out")
		case c.excluded:
			r.SetAddressHash(pii.HashAddress(c.address)).SetStatus(entrecipient.StatusSkipped).SetError("left out by the sender")
		default:
			r.SetAddress(c.address).SetAddressHash(pii.HashAddress(c.address))
		}
		builders = append(builders, r)
	}
	return e.Client.BroadcastRecipient.CreateBulk(builders...).
		OnConflict(sql.ConflictColumns(entrecipient.FieldBroadcastID, entrecipient.FieldChannel, entrecipient.FieldAddressHash)).
		DoNothing().Exec(ctx)
}

// publishBanner puts an in_app broadcast's banner live as an Announcement for its sender.
func (e *Engine) publishBanner(ctx context.Context, b *ent.Broadcast) error {
	content := ContentOf(b)
	if content.InApp == nil || !contains(b.Channels, "in_app") {
		return nil
	}
	if id, _ := b.Metadata["announcement_id"].(string); id != "" {
		return nil
	}
	ia := content.InApp
	days := ia.Days
	if days <= 0 {
		days = 7
	}
	ends := time.Now().AddDate(0, 0, days)
	audience := entannouncement.AudienceAll
	if ia.Audience == "admins" {
		audience = entannouncement.AudienceAdmins
	}
	tone := entannouncement.ToneInfo
	switch ia.Tone {
	case "feature":
		tone = entannouncement.ToneFeature
	case "warning":
		tone = entannouncement.ToneWarning
	}
	c := e.Client.Announcement.Create().
		SetTitle(ia.Title).SetSummary(ia.Summary).SetServices(ia.Services).SetAudience(audience).SetTone(tone).
		SetEndsAt(ends).SetCreatedBy("broadcast:" + b.ID.String()).SetMetadata(map[string]any{"broadcast_id": b.ID.String()})
	if ia.CTALabel != "" && ia.CTAURL != "" {
		c.SetCtaLabel(ia.CTALabel).SetCtaURL(ia.CTAURL)
	}
	if b.TenantID != nil {
		c.SetTenantID(*b.TenantID)
	}
	a, err := c.Save(ctx)
	if err != nil {
		return err
	}
	meta := cloneMap(b.Metadata)
	meta["announcement_id"] = a.ID.String()
	return e.Client.Broadcast.UpdateOneID(b.ID).SetMetadata(meta).Exec(ctx)
}

// ----------------------------------------------------------------------------------------------
// Dispatcher: pending recipients become queued messages, paced per channel.

type claimedRow struct {
	ID          uuid.UUID
	Channel     string
	Address     string
	DisplayName string
	Vars        map[string]any
}

func (e *Engine) dispatchTick(ctx context.Context) {
	if e.Pool == nil {
		return
	}
	sending, err := e.Client.Broadcast.Query().Where(entbroadcast.StatusEQ(entbroadcast.StatusSending)).
		Order(ent.Asc(entbroadcast.FieldUpdatedAt)).Limit(50).All(ctx)
	if err != nil {
		e.Log.Warn("dispatcher: query failed", zap.Error(err))
		return
	}
	for _, b := range sending {
		if b.Class == entbroadcast.ClassMarketing && !inSendWindow(b, time.Now()) {
			continue
		}
		e.dispatchBroadcast(ctx, b)
	}
}

func (e *Engine) dispatchBroadcast(ctx context.Context, b *ent.Broadcast) {
	content := ContentOf(b)
	sentAny := false
	for _, ch := range deliverableChannels(b.Channels) {
		budget := e.budget(ctx, b, ch)
		if budget == 0 {
			continue
		}
		rows, err := e.claim(ctx, b.ID, ch, budget)
		if err != nil {
			e.Log.Warn("dispatcher: claim failed", zap.String("broadcast", b.ID.String()), zap.Error(err))
			continue
		}
		for _, r := range rows {
			msg, err := e.buildMessage(b, content, r)
			if err == nil {
				_, err = messaging.Publish(ctx, e.NATS, e.Events, msg)
			}
			if err != nil {
				e.Log.Warn("dispatcher: publish failed", zap.String("recipient", r.ID.String()), zap.Error(err))
				_ = e.Client.BroadcastRecipient.UpdateOneID(r.ID).SetStatus(entrecipient.StatusPending).
					SetNextAttemptAt(time.Now().Add(time.Minute)).SetError(truncate(err.Error())).Exec(ctx)
				continue
			}
			sentAny = true
		}
	}
	if !sentAny {
		e.maybeComplete(ctx, b)
	}
}

// budget is how many messages this channel may queue now: the pacing limiter's tokens, taken one
// at a time up to the batch size. Keyed by the sending account (the platform's, or the tenant's).
func (e *Engine) budget(ctx context.Context, b *ent.Broadcast, channel string) int {
	opts, ok := defaultPace[channel]
	if !ok || e.Limiter == nil {
		return claimBatch
	}
	account := e.PlatformID
	if b.TenantID != nil {
		account = b.TenantID.String()
	}
	n := 0
	for n < claimBatch {
		allowed, _ := e.Limiter.Allow(ctx, "broadcast:"+channel+":"+account, opts, 1)
		if !allowed {
			break
		}
		n++
	}
	return n
}

// claim marks up to n pending rows as dispatching and returns them. SKIP LOCKED lets several
// worker pods claim from the same broadcast without ever taking the same row.
func (e *Engine) claim(ctx context.Context, broadcastID uuid.UUID, channel string, n int) ([]claimedRow, error) {
	rows, err := e.Pool.Query(ctx, `
		UPDATE broadcast_recipients SET status = 'dispatching', attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM broadcast_recipients
			WHERE broadcast_id = $1 AND channel = $2 AND status = 'pending'
			  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
			ORDER BY id
			LIMIT $3
			FOR UPDATE SKIP LOCKED)
		RETURNING id, channel, coalesce(address, ''), coalesce(display_name, ''), coalesce(vars, '{}'::jsonb)`,
		broadcastID, channel, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimedRow
	for rows.Next() {
		var r claimedRow
		var raw []byte
		if err := rows.Scan(&r.ID, &r.Channel, &r.Address, &r.DisplayName, &raw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &r.Vars)
		out = append(out, r)
	}
	return out, rows.Err()
}

// buildMessage renders one recipient's message for the worker.
func (e *Engine) buildMessage(b *ent.Broadcast, content Content, r claimedRow) (messaging.Message, error) {
	vars := VarsFromMap(r.Vars)
	marketing := b.Class == entbroadcast.ClassMarketing
	sender := e.PlatformID
	scope := messaging.SenderScopePlatform
	if b.TenantID != nil {
		sender = b.TenantID.String()
		scope = messaging.SenderScopeTenant
	}
	msg := messaging.Message{
		TenantID:       sender,
		Channel:        r.Channel,
		TemplateID:     TemplateID,
		SenderScope:    scope,
		Target:         messaging.TargetCustomer,
		To:             []string{r.Address},
		Data:           map[string]any{},
		Metadata:       map[string]any{MetaRecipientID: r.ID.String(), MetaBroadcastID: b.ID.String()},
		RequestID:      "bcr-" + r.ID.String(),
		IdempotencyKey: "broadcast-recipient-" + r.ID.String(),
		QueuedAt:       time.Now(),
	}
	if b.TenantID == nil {
		msg.Target = messaging.TargetTenantAdmin
	}
	unsubscribe := ""
	if marketing && e.Suppress != nil && (r.Channel == "email") {
		unsubscribe = strings.TrimRight(e.PublicURL, "/") + "/api/v1/public/unsubscribe/" + e.Suppress.TokenFor(b.TenantID, r.Channel, r.Address, vars.SenderName)
	}
	switch r.Channel {
	case "email":
		if content.Email == nil {
			return msg, errors.New("no email content")
		}
		body := Render(content.Email.Body, vars)
		msg.Metadata["subject"] = Render(content.Email.Subject, vars)
		msg.Data["paragraphs"] = paragraphs(body)
		msg.Data["sender_name"] = vars.SenderName
		msg.Data["brand_name"] = vars.SenderName
		if unsubscribe != "" {
			msg.Data["unsubscribe_url"] = unsubscribe
			msg.Metadata[MetaUnsubscribe] = unsubscribe
		}
	case "sms":
		if content.SMS == nil {
			return msg, errors.New("no sms content")
		}
		body := Render(content.SMS.Body, vars)
		if marketing {
			body = WithSMSOptOut(body)
		}
		msg.Data["body"] = body
	case "whatsapp":
		w := content.WhatsApp
		if w == nil {
			return msg, errors.New("no whatsapp content")
		}
		params := make([]string, 0, len(w.Params))
		for _, p := range w.Params {
			params = append(params, whatsappParam(p, vars, w.Message))
		}
		msg.Metadata["template_name"] = w.Template
		msg.Metadata["template_params"] = params
		msg.Data["body"] = Render(w.Message, vars)
		if backups := stringList(r.Vars["backups"]); len(backups) > 0 {
			msg.Metadata[messaging.MetaFallbackTo] = backups
		}
	}
	return msg, nil
}

func whatsappParam(token string, v Vars, message string) string {
	switch token {
	case "message":
		// Template variables cannot hold newlines; keep paragraphs readable on one line.
		return strings.Join(strings.Fields(Render(message, v)), " ")
	default:
		return Render("{"+token+"}", v)
	}
}

func paragraphs(body string) []string {
	var out []string
	for _, p := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// maybeComplete closes a broadcast once its audience is fully materialised and nothing is
// pending or in flight.
func (e *Engine) maybeComplete(ctx context.Context, b *ent.Broadcast) {
	fresh, err := e.Client.Broadcast.Get(ctx, b.ID)
	if err != nil {
		return
	}
	if done, _ := fresh.Metadata["materialised"].(bool); !done {
		return
	}
	open, err := e.Client.BroadcastRecipient.Query().Where(entrecipient.BroadcastID(b.ID),
		entrecipient.StatusIn(entrecipient.StatusPending, entrecipient.StatusDispatching)).Exist(ctx)
	if err != nil || open {
		_ = RefreshCounts(ctx, e.Client, b.ID)
		return
	}
	n, err := e.Client.Broadcast.Update().Where(entbroadcast.ID(b.ID), entbroadcast.StatusEQ(entbroadcast.StatusSending)).
		SetStatus(entbroadcast.StatusCompleted).SetCompletedAt(time.Now()).Save(ctx)
	if err != nil || n == 0 {
		return
	}
	_ = RefreshCounts(ctx, e.Client, b.ID)
	e.publishCompleted(b)
}

func (e *Engine) publishCompleted(b *ent.Broadcast) {
	fresh, err := e.Client.Broadcast.Get(context.Background(), b.ID)
	if err != nil {
		fresh = b
	}
	payload := map[string]any{
		"broadcast_id": b.ID.String(), "title": b.Title, "kind": string(b.Kind), "status": "completed",
		"target": fresh.TargetCount, "sent": fresh.SentCount, "failed": fresh.FailedCount,
		"skipped": fresh.SkippedCount, "suppressed": fresh.SuppressedCount,
	}
	// A broadcast another service handed over carries its source, so that service can match the
	// completion to its own record (a Maskani notice, a MarketFlow campaign).
	for _, k := range []string{"source", "source_ref"} {
		if v, ok := fresh.Metadata[k].(string); ok && v != "" {
			payload[k] = v
		}
	}
	PublishEvent(e.NATS, e.Log, SubjectCompleted, "broadcast.completed", b.TenantID, b.ID, payload)
}

// reapStale returns rows stuck in dispatching (a pod died after claiming) to pending, and
// releases broadcasts whose window wait is long past.
func (e *Engine) reapStale(ctx context.Context) {
	cutoff := time.Now().Add(-staleDispatching)
	n, err := e.Client.BroadcastRecipient.Update().
		Where(entrecipient.StatusEQ(entrecipient.StatusDispatching), entrecipient.UpdatedAtLT(cutoff), entrecipient.AttemptsLT(maxAttempts)).
		SetStatus(entrecipient.StatusPending).Save(ctx)
	if err == nil && n > 0 {
		e.Log.Info("requeued stale broadcast recipients", zap.Int("count", n))
	}
	_, _ = e.Client.BroadcastRecipient.Update().
		Where(entrecipient.StatusEQ(entrecipient.StatusDispatching), entrecipient.UpdatedAtLT(cutoff), entrecipient.AttemptsGTE(maxAttempts)).
		SetStatus(entrecipient.StatusFailed).SetError("no result from the sender after several tries").Save(ctx)
}

// inSendWindow keeps marketing sends to daytime in the sender's timezone (default 08:00-20:00).
func inSendWindow(b *ent.Broadcast, now time.Time) bool {
	tz, _ := b.Metadata["timezone"].(string)
	loc := mustLoc(tz)
	h := now.In(loc).Hour()
	from, to := defaultWindowFrom, defaultWindowTo
	if w, ok := b.Metadata["send_window"].(map[string]any); ok {
		if f, ok := w["from"].(float64); ok {
			from = int(f)
		}
		if t, ok := w["to"].(float64); ok {
			to = int(t)
		}
	}
	return h >= from && h < to
}

// ----------------------------------------------------------------------------------------------
// Outcomes, recorded by the worker after each attempt.

// RecordOutcome stores the result of one broadcast message. status is sent, failed or skipped.
// A failed send is retried (back to pending with a growing delay) until maxAttempts.
func RecordOutcome(ctx context.Context, client *ent.Client, msg *messaging.Message, status string, sendErr error) {
	if client == nil || msg == nil || msg.Metadata == nil {
		return
	}
	idStr, _ := msg.Metadata[MetaRecipientID].(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return
	}
	row, err := client.BroadcastRecipient.Get(ctx, id)
	if err != nil || row.Status != entrecipient.StatusDispatching {
		return
	}
	up := client.BroadcastRecipient.Update().Where(entrecipient.ID(id), entrecipient.StatusEQ(entrecipient.StatusDispatching))
	switch status {
	case "sent":
		up.SetStatus(entrecipient.StatusSent).SetSentAt(time.Now()).ClearError()
		if mid, _ := msg.Metadata[messaging.MetaSentMessageID].(string); mid != "" {
			up.SetProviderMessageID(mid)
		}
	case "skipped":
		// The worker passes its specific reason (unverified email, no SMS credit, ...).
		reason := "not sent"
		if sendErr != nil && sendErr.Error() != "" {
			reason = truncate(sendErr.Error())
		}
		up.SetStatus(entrecipient.StatusSkipped).SetError(reason)
	default:
		reason := "send failed"
		if sendErr != nil {
			reason = truncate(sendErr.Error())
		}
		if row.Attempts < maxAttempts && !permanent(sendErr) {
			up.SetStatus(entrecipient.StatusPending).SetError(reason).
				SetNextAttemptAt(time.Now().Add(time.Duration(row.Attempts*row.Attempts) * 5 * time.Minute))
		} else {
			up.SetStatus(entrecipient.StatusFailed).SetError(reason)
		}
	}
	_, _ = up.Save(ctx)
}

// permanent is a provider error retrying cannot fix (a bad address, an opted-out or blocked
// number, Meta's per-person marketing cap).
func permanent(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{"131049", "131026", "131050", "invalid", "blacklist", "not a valid", "550 5.1", "user unknown", "does not exist"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func truncate(s string) string {
	if len(s) > 480 {
		return s[:480]
	}
	return s
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func mustLoc(name string) *time.Location {
	if name == "" {
		name = "Africa/Nairobi"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.FixedZone("EAT", 3*3600)
}
