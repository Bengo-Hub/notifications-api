package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// User holds the schema definition for the User entity.
type User struct {
	ent.Schema
}

// Fields of the User.
func (User) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("auth_service_user_id", uuid.UUID{}).
			Optional().
			Unique().
			Comment("Reference to auth-service user. Identity data synced from auth-service."),
		field.String("email").
			NotEmpty(),
		// email_verified mirrors auth-service's durable users.email_verified (carried on
		// auth.user.created/updated). The email delivery gate drops mail to a KNOWN user
		// whose email is unverified; addresses with no local user row are unaffected.
		field.Bool("email_verified").
			Default(false).
			Comment("Mirror of auth-service users.email_verified; gates outbound email"),
		field.String("sync_status").
			Default("pending").
			Comment("Sync status with auth-service: pending, synced, failed"),
		field.Time("sync_at").
			Optional().
			Comment("Last sync timestamp with auth-service"),
		field.String("full_name").
			NotEmpty(),
		field.String("phone").
			Optional(),
		field.String("status").
			Default("active"),
		field.String("primary_role").
			Optional(),
		field.String("locale").
			Default("en"),
		field.Time("last_login_at").
			Optional(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the User.
func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("users").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("roles", Role.Type),
	}
}

// Indexes of the User.
func (User) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "email").
			Unique(),
		index.Fields("tenant_id", "auth_service_user_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("email"),
		index.Fields("sync_status"),
		index.Fields("auth_service_user_id"),
		index.Fields("created_at"),
	}
}
