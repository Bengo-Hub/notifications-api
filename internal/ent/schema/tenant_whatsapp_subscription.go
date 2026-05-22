package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// TenantWhatsAppSubscription holds the schema definition for tenant WhatsApp subscriptions.
type TenantWhatsAppSubscription struct {
	ent.Schema
}

// Fields of the TenantWhatsAppSubscription.
func (TenantWhatsAppSubscription) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Tenant identifier"),
		field.UUID("plan_id", uuid.UUID{}).
			Comment("WhatsApp plan identifier"),
		field.Enum("status").
			Values("active", "cancelled", "expired", "trial").
			Default("trial").
			Comment("Current subscription status"),
		field.Time("started_at").
			Default(time.Now).
			Comment("Subscription start date"),
		field.Time("expires_at").
			Comment("Subscription expiry date"),
		field.String("payment_reference").
			Optional().
			Comment("Treasury payment reference for this subscription period"),
		field.Bool("auto_renew").
			Default(true).
			Comment("Whether subscription auto-renews on expiry"),
		field.Int("messages_used").
			Default(0).
			Comment("Messages sent in the current billing period"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the TenantWhatsAppSubscription.
func (TenantWhatsAppSubscription) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("plan", WhatsAppPlan.Type).
			Field("plan_id").
			Required().
			Unique(),
	}
}

// Indexes of the TenantWhatsAppSubscription.
func (TenantWhatsAppSubscription) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("tenant_id", "status"),
	}
}
