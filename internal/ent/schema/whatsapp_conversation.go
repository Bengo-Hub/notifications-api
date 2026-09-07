package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// WhatsAppConversation holds the schema definition for a tenant's WhatsApp conversation thread
// with one customer. Meta's Cloud API numbers can't be used from the regular WhatsApp app once
// registered — this table (plus WhatsAppMessage) is what lets tenant staff actually see and reply
// to a customer's messages through our own UI instead.
type WhatsAppConversation struct {
	ent.Schema
}

// Fields of the WhatsAppConversation.
func (WhatsAppConversation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Tenant identifier"),
		field.String("phone_number_id").
			NotEmpty().
			Comment("Meta phone_number_id this conversation belongs to"),
		field.String("customer_wa_id").
			NotEmpty().
			Comment("Customer's WhatsApp ID (digits, no leading +)"),
		field.String("customer_name").
			Optional().
			Comment("Customer's WhatsApp profile name, when Meta supplies it"),
		field.Time("last_message_at").
			Default(time.Now).
			Comment("Timestamp of the most recent message, either direction"),
		field.String("last_message_preview").
			Optional().
			Comment("Truncated text of the most recent message"),
		field.Time("last_inbound_at").
			Optional().
			Nillable().
			Comment("Timestamp of the customer's last inbound message only — outbound replies never advance this. Meta's 24h free-form reply window is computed from it"),
		field.Int("unread_count").
			Default(0),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the WhatsAppConversation.
func (WhatsAppConversation) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("messages", WhatsAppMessage.Type).
			Ref("conversation"),
	}
}

// Indexes of the WhatsAppConversation.
func (WhatsAppConversation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "phone_number_id", "customer_wa_id").Unique(),
		index.Fields("tenant_id", "last_message_at"),
	}
}
