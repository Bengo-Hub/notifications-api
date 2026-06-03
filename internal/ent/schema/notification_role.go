package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// NotificationRole holds the schema definition for notification service roles.
type NotificationRole struct {
	ent.Schema
}

// Fields of the NotificationRole.
func (NotificationRole) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("NULL = global/system role shared platform-wide across all tenants; non-NULL = tenant-specific custom role"),
		field.String("role_code").
			NotEmpty().
			Comment("Role code: notifications_admin, operator, viewer"),
		field.String("name").
			NotEmpty().
			Comment("Display name"),
		field.Text("description").
			Optional(),
		field.Bool("is_system_role").
			Default(false).
			Comment("System roles cannot be deleted"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the NotificationRole.
func (NotificationRole) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("permissions", NotificationPermission.Type).Through("notification_role_permissions", NotificationRolePermission.Type),
		edge.To("user_assignments", UserRoleAssignment.Type),
	}
}

// Indexes of the NotificationRole.
func (NotificationRole) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("tenant_id", "role_code").Unique(),
		// Fast lookup of a role by code regardless of tenant (global resolution path).
		index.Fields("role_code"),
		index.Fields("is_system_role"),
	}
}
