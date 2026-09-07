package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// WhatsAppMessage holds the schema definition for one message within a WhatsAppConversation,
// either direction.
type WhatsAppMessage struct {
	ent.Schema
}

// Fields of the WhatsAppMessage.
func (WhatsAppMessage) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("conversation_id", uuid.UUID{}).
			Comment("Owning conversation"),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Denormalized for defense-in-depth tenant filtering on every read"),
		field.Enum("direction").
			Values("inbound", "outbound").
			Comment("inbound = from the customer, outbound = a staff reply"),
		field.String("message_type").
			Default("text").
			Comment("Meta's messages[].type — only \"text\" renders in v1, others get a placeholder body"),
		field.Text("body").
			NotEmpty(),
		field.String("wa_message_id").
			Optional().
			Comment("Meta's wamid — the webhook redelivery dedup key"),
		field.String("status").
			Default("received").
			Comment("received (inbound) | queued/sent/delivered/read/failed (outbound, via the status webhook)"),
		field.UUID("sent_by_user_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Auth-service user ID of the staff member who sent an outbound reply; nil for inbound"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the WhatsAppMessage.
func (WhatsAppMessage) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("conversation", WhatsAppConversation.Type).
			Field("conversation_id").
			Required().
			Unique(),
	}
}

// Indexes of the WhatsAppMessage.
func (WhatsAppMessage) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("conversation_id", "created_at"),
		index.Fields("tenant_id"),
		index.Fields("wa_message_id").Unique(),
	}
}
