package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Occasion is a date that repeats every year (a public holiday, Customer Service Week, a
// business anniversary) with the wording used to greet people on it. Platform rows (null
// tenant_id) are the shared catalogue; a tenant row either customises a catalogue occasion
// (same key) or adds the tenant's own. The planner turns an upcoming occasion into a Broadcast.
type Occasion struct {
	ent.Schema
}

// Fields of the Occasion.
func (Occasion) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		field.String("key").
			NotEmpty().
			MaxLen(80).
			Comment("Stable slug, e.g. customer_service_week; a tenant row with a catalogue key customises it"),
		field.String("name").
			NotEmpty().
			MaxLen(120),
		field.String("country").
			Default("KE").
			Comment("ISO country the date belongs to; empty for worldwide"),
		field.JSON("rule", map[string]any{}).
			Comment("fixed{month,day} | nth_weekday{month,weekday,n} | first_full_week{month} | easter_offset{days} | explicit{dates:{year:date}}"),
		field.Int("duration_days").
			Default(1),
		field.Int("lead_days").
			Default(14).
			Comment("How many days ahead the draft is prepared for approval"),
		field.Int("send_offset_days").
			Default(0).
			Comment("Which day of the occasion to send on (0 = first day)"),
		field.Bool("is_active").
			Default(true),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("variants, channels, audience, auto_send, send_time, whatsapp_template, last_variant"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the Occasion.
func (Occasion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("key").
			Unique().
			Annotations(entsql.IndexWhere("tenant_id IS NULL")).
			StorageKey("occasion_platform_key"),
		index.Fields("tenant_id", "key").
			Unique().
			Annotations(entsql.IndexWhere("tenant_id IS NOT NULL")).
			StorageKey("occasion_tenant_key"),
	}
}
