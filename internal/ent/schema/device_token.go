package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DeviceToken holds the schema definition for the DeviceToken entity.
// Stores FCM/APNS push notification device tokens per user.
type DeviceToken struct {
	ent.Schema
}

// Fields of the DeviceToken.
func (DeviceToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Tenant identifier"),
		field.UUID("user_id", uuid.UUID{}).
			Comment("Auth-service user ID"),
		field.Text("token").
			NotEmpty().
			Comment("FCM or APNS device token"),
		field.String("platform").
			Default("android").
			Comment("Device platform: android, ios, web"),
		field.String("provider").
			Default("fcm").
			Comment("Push provider: fcm, apns"),
		field.Bool("is_active").
			Default(true),
		field.Time("last_used_at").
			Optional().
			Comment("Last time a push was sent to this token"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the DeviceToken.
func (DeviceToken) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "user_id"),
		index.Fields("token").Unique(),
		index.Fields("is_active"),
	}
}
