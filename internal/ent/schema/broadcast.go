package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Broadcast is one notice sent to many people: the platform to its tenants, or a tenant to its
// customers or staff. It can go out on several channels at once (email, sms, whatsapp, push and
// an in-app banner), is approved before it sends, and is paced under each provider's limits.
// Yearly occasion messages (Customer Service Week, New Year) are broadcasts generated from an
// Occasion.
type Broadcast struct {
	ent.Schema
}

// Fields of the Broadcast.
func (Broadcast) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Enum("scope").
			Values("platform", "tenant").
			Immutable().
			Comment("platform: sent by the platform owner; tenant: sent by tenant_id"),
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable().
			Comment("Sending tenant; null for platform broadcasts"),
		field.Enum("kind").
			Values("announcement", "greeting", "marketing", "service_notice").
			Default("announcement"),
		field.Enum("class").
			Values("marketing", "transactional").
			Default("marketing").
			Comment("marketing needs consent and carries an opt-out; transactional does not"),
		field.Enum("status").
			Values("draft", "pending_approval", "scheduled", "sending", "paused", "completed", "cancelled", "rejected", "failed").
			Default("draft"),
		field.String("title").
			NotEmpty().
			MaxLen(160).
			Comment("Internal name shown in lists"),
		field.JSON("channels", []string{}).
			Comment("email, sms, whatsapp, push, in_app"),
		field.JSON("content", map[string]any{}).
			Comment("Per channel content: email{subject,body}, sms{body}, whatsapp{template,params}, push{title,body}, in_app{banner fields}"),
		field.JSON("audience", map[string]any{}).
			Comment("{type: platform_tenants|tenant_customers|tenant_users|manual, filters...}"),
		field.Time("send_at").
			Optional().
			Nillable().
			Comment("When to start sending; null sends as soon as it is approved"),
		field.UUID("occasion_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Int("occasion_year").
			Optional().
			Nillable(),
		field.String("requested_by").
			Optional(),
		field.String("approved_by").
			Optional(),
		field.Time("approved_at").
			Optional().
			Nillable(),
		field.Int("target_count").Default(0),
		field.Int("sent_count").Default(0),
		field.Int("failed_count").Default(0),
		field.Int("skipped_count").Default(0),
		field.Int("suppressed_count").Default(0),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("Send window, timezone, estimate, materialiser cursor, variant index, AI draft provenance"),
		field.Time("completed_at").
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

// Indexes of the Broadcast.
func (Broadcast) Indexes() []ent.Index {
	return []ent.Index{
		// Lists per sender, and the scheduler's "due" scan.
		index.Fields("tenant_id", "status", "send_at"),
		index.Fields("status", "send_at"),
		// One generated broadcast per occasion per year per sender, so concurrent planners can
		// never create two. Platform rows have a null tenant_id, hence two partial indexes.
		index.Fields("occasion_id", "occasion_year").
			Unique().
			Annotations(entsql.IndexWhere("tenant_id IS NULL AND occasion_id IS NOT NULL")).
			StorageKey("broadcast_platform_occasion_year"),
		index.Fields("tenant_id", "occasion_id", "occasion_year").
			Unique().
			Annotations(entsql.IndexWhere("tenant_id IS NOT NULL AND occasion_id IS NOT NULL")).
			StorageKey("broadcast_tenant_occasion_year"),
	}
}
