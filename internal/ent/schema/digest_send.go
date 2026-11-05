package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DigestSend is one daily summary to one person: the marker that makes the summary go out once.
// The unique (tenant, kind, recipient, date) row is claimed before anything is sent, so a second
// pod or a restart never sends the day twice. channels lists the channels already queued, so a pod
// that dies halfway is resumed by the next run (after the claim's lease) without repeating them.
type DigestSend struct {
	ent.Schema
}

// Fields of the DigestSend.
func (DigestSend) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Immutable(),
		field.String("kind").
			NotEmpty().
			Immutable(),
		field.String("recipient_key").
			NotEmpty().
			Immutable(),
		field.String("local_date").
			NotEmpty().
			Immutable().
			Comment("YYYY-MM-DD of the summary, in the tenant's timezone"),
		field.String("status").
			Default("sending").
			Comment("sending (claimed), sent"),
		field.JSON("channels", []string{}).
			Optional().
			Comment("Channels already queued (or skipped for an opt-out), so a resumed run skips them"),
		field.Int64("item_count").
			Default(0),
		field.Time("claimed_at").
			Default(time.Now),
		field.Time("sent_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the DigestSend.
func (DigestSend) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "kind", "recipient_key", "local_date").
			Unique().
			StorageKey("digestsend_once_per_day"),
		index.Fields("created_at").
			StorageKey("digestsend_created_at"),
	}
}
