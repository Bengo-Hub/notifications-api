package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// BroadcastRecipient is one address on one channel of a broadcast, and its delivery outcome. Rows
// are written by the materialiser and claimed in batches by the dispatcher.
type BroadcastRecipient struct {
	ent.Schema
}

// Fields of the BroadcastRecipient.
func (BroadcastRecipient) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("broadcast_id", uuid.UUID{}).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable().
			Comment("Sending tenant (null for platform broadcasts)"),
		field.UUID("recipient_tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("For platform broadcasts: the tenant this recipient belongs to"),
		field.String("channel").
			NotEmpty().
			Immutable(),
		field.String("address").
			Optional().
			Nillable().
			Comment("Normalised address; cleared by the retention job after 30 days"),
		field.String("address_hash").
			NotEmpty().
			Immutable().
			Comment("SHA-256 of the normalised address (pii.HashAddress)"),
		field.String("display_name").
			Optional(),
		field.JSON("vars", map[string]any{}).
			Optional().
			Comment("Personalisation values: first_name, business_name, ..."),
		field.Enum("status").
			Values("pending", "dispatching", "sent", "delivered", "failed", "skipped", "suppressed").
			Default("pending"),
		field.String("provider_message_id").
			Optional(),
		field.String("error").
			Optional().
			MaxLen(500),
		field.Int("attempts").
			Default(0),
		field.Time("next_attempt_at").
			Optional().
			Nillable(),
		field.Time("sent_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the BroadcastRecipient.
func (BroadcastRecipient) Indexes() []ent.Index {
	return []ent.Index{
		// One row per address per channel per broadcast: re-running the materialiser is a no-op.
		index.Fields("broadcast_id", "channel", "address_hash").Unique(),
		// The dispatcher claims pending rows per broadcast in id order; the detail view counts
		// and pages by status.
		index.Fields("broadcast_id", "status", "id"),
		// Provider delivery callbacks find their row.
		index.Fields("provider_message_id"),
		// Retention scans.
		index.Fields("created_at"),
	}
}
