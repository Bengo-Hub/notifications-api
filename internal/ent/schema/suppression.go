package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Suppression stops messages to an address: an unsubscribe (marketing), a STOP reply, or a hard
// bounce (all). It is per sender, so opting out of one business's offers does not silence
// another's receipts. Only the address hash is kept.
type Suppression struct {
	ent.Schema
}

// Fields of the Suppression.
func (Suppression) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable().
			Comment("Sender whose messages are stopped; null = the platform"),
		field.String("channel").
			NotEmpty().
			Immutable().
			Comment("email, sms, whatsapp"),
		field.String("address_hash").
			NotEmpty().
			Immutable(),
		field.Enum("scope").
			Values("marketing", "all").
			Default("marketing").
			Immutable().
			Comment("marketing: offers and greetings only; all: everything except locked security messages"),
		field.String("reason").
			Default("unsubscribe").
			Comment("unsubscribe, stop_reply, hard_bounce, complaint, manual"),
		field.String("source").
			Optional().
			Comment("email_link, one_click, sms_inbound, whatsapp_button, admin"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the Suppression.
func (Suppression) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("channel", "address_hash", "scope").
			Unique().
			Annotations(entsql.IndexWhere("tenant_id IS NULL")).
			StorageKey("suppression_platform_address"),
		index.Fields("tenant_id", "channel", "address_hash", "scope").
			Unique().
			Annotations(entsql.IndexWhere("tenant_id IS NOT NULL")).
			StorageKey("suppression_tenant_address"),
	}
}
