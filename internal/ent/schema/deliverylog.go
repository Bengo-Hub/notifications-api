package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DeliveryLog holds the schema definition for notification delivery audit log.
type DeliveryLog struct {
	ent.Schema
}

// Fields of the DeliveryLog.
func (DeliveryLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("tenant_id").
			NotEmpty(),
		field.String("template_id").
			NotEmpty().
			Comment("Template identifier (e.g. order_confirmation)"),
		field.String("channel").
			NotEmpty().
			Comment("email, sms, push"),
		field.String("recipient").
			NotEmpty(),
		field.String("status").
			Default("sent").
			Comment("sent, delivered, failed, skipped"),
		field.String("message_id").
			Optional().
			Comment("The queued message's request id, so provider callbacks can find the row"),
		field.String("provider_message_id").
			Optional().
			Comment("The provider's own id for the send (Meta's wamid), so its status webhook can update the row"),
		field.String("error_code").
			Optional().
			Comment("Provider error code of a send the provider later refused (Meta 131049, 131026 ...)"),
		field.String("error_detail").
			Optional().
			Comment("Provider's reason for that refusal, in its words"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the DeliveryLog. (tenant_id, created_at) serves every tenant query, so there is no
// separate tenant_id index; created_at alone serves the platform-wide view and retention.
func (DeliveryLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		index.Fields("tenant_id", "created_at"),
		index.Fields("message_id"),
		index.Fields("provider_message_id"),
	}
}
