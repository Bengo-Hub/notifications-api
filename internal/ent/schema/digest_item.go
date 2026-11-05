package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DigestItem is one message held back for a daily summary instead of being sent at once (Hadia's
// owner gift digest, platform task N3). It is keyed by the source event, so a redelivered event
// adds nothing. send_id is empty until a summary takes the item; the summary job fills it in the
// same statement that picks the items, so an item is in exactly one summary. Rows are removed a
// week after their summary went out (the data holds the recipient's contact details).
type DigestItem struct {
	ent.Schema
}

// Fields of the DigestItem.
func (DigestItem) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Immutable(),
		field.String("kind").
			NotEmpty().
			Immutable().
			Comment("What the summary is about, e.g. hadia_gift"),
		field.String("recipient_key").
			NotEmpty().
			Immutable().
			Comment("Hash naming the person the summary goes to (never the address itself)"),
		field.String("local_date").
			NotEmpty().
			Immutable().
			Comment("YYYY-MM-DD of the summary the item belongs to, in the tenant's timezone"),
		field.String("group_key").
			Default("").
			Immutable().
			Comment("One line of the summary, e.g. the registry id"),
		field.String("event_id").
			NotEmpty().
			Immutable().
			Comment("Source event id; unique per tenant and kind"),
		field.Int64("amount_minor").
			Default(0).
			Immutable().
			Comment("Amount in minor units (cents)"),
		field.JSON("data", map[string]any{}).
			Optional().
			Comment("What the summary needs: names, titles, links, the recipient's email and phone"),
		field.UUID("send_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("The digest_sends row whose summary took this item"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the DigestItem.
func (DigestItem) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "kind", "event_id").
			Unique().
			StorageKey("digestitem_tenant_kind_event"),
		index.Fields("kind", "tenant_id", "recipient_key", "local_date").
			Annotations(entsql.IndexWhere("send_id IS NULL")).
			StorageKey("digestitem_pending"),
		index.Fields("send_id").
			StorageKey("digestitem_send_id"),
		index.Fields("created_at").
			StorageKey("digestitem_created_at"),
	}
}
