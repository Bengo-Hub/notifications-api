package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ProviderSetting holds the schema definition for the ProviderSetting entity.
type ProviderSetting struct {
	ent.Schema
}

// Fields of the ProviderSetting.
func (ProviderSetting) Fields() []ent.Field {
	return []ent.Field{
		field.String("tenant_id"),
		field.String("channel"). // email, sms, push
						Optional(),
		field.String("provider"). // smtp, brevo, africastalking, meta_cloud, fcm
						Optional(),
		field.String("provider_type"). // email, sms, push
						Optional(),
		field.String("provider_name"). // smtp, brevo, africastalking, meta_cloud, fcm
						Optional(),
		field.String("key").
			Optional(),
		field.Text("value").
			Optional(),
		field.Text("description").
			Optional(),
		field.Bool("is_encrypted").
			Default(false),
		field.Bool("is_platform").
			Default(false).
			Comment("Platform-level provider config (associated with platform owner tenant UUID)"),
		field.Bool("is_platform_managed").
			Default(false).
			Comment("If true, only platform owners can manage this setting, and it applies to all tenants"),
		field.String("environment").
			Default("production").
			Comment("sandbox, production"),
		field.Bool("is_secret").
			Default(false).
			Comment("Whether this setting's value should be masked in the UI"),
		field.Bool("is_active").
			Default(true).
			Comment("Whether this provider config is currently active"),
		field.String("status").
			Default("active").
			Optional().
			Comment("Provider status: active, fallback, error"),
	}
}

// Edges of the ProviderSetting.
func (ProviderSetting) Edges() []ent.Edge {
	return nil
}

// Indexes of the ProviderSetting.
func (ProviderSetting) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "environment", "provider_type", "provider_name", "key").
			Unique(),
		index.Fields("tenant_id", "environment", "provider_type"),
	}
}
