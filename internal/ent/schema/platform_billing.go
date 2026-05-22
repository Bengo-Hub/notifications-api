package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// PlatformBilling holds the schema definition for global billing settings.
type PlatformBilling struct {
	ent.Schema
}

// Fields of the PlatformBilling.
func (PlatformBilling) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Float("cost_per_sms").
			Default(1.0).
			Comment("Tenant-facing rate per SMS segment (KES)"),
		field.Float("cost_per_whatsapp").
			Default(2.0).
			Comment("Base WhatsApp monthly subscription rate (KES) — used when no plan is set"),
		field.Float("provider_cost_per_sms").
			Default(0.5).
			Comment("Actual provider (AT) cost per SMS segment — platform profit = cost_per_sms - provider_cost_per_sms"),
		field.Float("provider_cost_per_whatsapp").
			Default(0.8).
			Comment("Estimated provider cost per WhatsApp message — for internal profit tracking"),
		field.Float("min_markup_percentage").
			Default(40.0).
			Comment("Minimum required markup percentage above provider cost (e.g. 40 = 40%)"),
		field.Float("min_topup_amount").
			Default(500.0).
			Comment("Minimum allowed top-up amount in base currency"),
		field.UUID("treasury_gateway_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("The gateway ID in Treasury service to use for top-ups"),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}
