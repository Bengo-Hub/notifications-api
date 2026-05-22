package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// WhatsAppPlan defines subscription tiers for WhatsApp messaging.
type WhatsAppPlan struct {
	ent.Schema
}

func (WhatsAppPlan) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty().
			Comment("Plan display name (e.g. Basic, Standard, Pro)"),
		field.String("slug").
			Unique().
			NotEmpty().
			Comment("URL-safe unique identifier (e.g. basic, standard, pro)"),
		field.Float("price_monthly").
			Comment("Monthly subscription price in KES charged to tenant"),
		field.Float("provider_cost").
			Default(0).
			Comment("Estimated monthly provider cost — platform profit = price_monthly - provider_cost"),
		field.Int("messages_per_month").
			Default(0).
			Comment("Message quota per month; 0 = unlimited"),
		field.Bool("is_active").
			Default(true).
			Comment("Whether this plan is available for new subscriptions"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (WhatsAppPlan) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("subscriptions", TenantWhatsAppSubscription.Type).
			Ref("plan"),
	}
}
