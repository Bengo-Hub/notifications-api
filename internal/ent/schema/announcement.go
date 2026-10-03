package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Announcement is a platform-wide "what's new" banner the platform admin publishes to the apps'
// dashboards (a new payment gateway, a new module). Apps read the active ones for their service
// from a public endpoint; each user dismisses them on their own device.
type Announcement struct {
	ent.Schema
}

// Fields of the Announcement.
func (Announcement) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("title").
			NotEmpty().
			MaxLen(120),
		field.Text("summary").
			NotEmpty().
			Comment("One or two sentences shown on the banner"),
		field.JSON("highlights", []string{}).
			Optional().
			Comment("Short feature bullets shown when the banner is expanded"),
		field.String("cta_label").
			Optional(),
		field.String("cta_url").
			Optional().
			Comment("Absolute URL or app path; {orgSlug} is replaced with the viewer's organization slug"),
		field.JSON("services", []string{}).
			Optional().
			Comment("App keys that show it (pos, treasury, inventory, ...); empty means every app"),
		field.Enum("audience").
			Values("all", "admins").
			Default("all").
			Comment("admins: shown only to users who can change settings"),
		field.Enum("tone").
			Values("feature", "info", "warning").
			Default("feature"),
		field.Int("priority").
			Default(0).
			Comment("Higher shows first"),
		field.Bool("dismissible").
			Default(true),
		field.Bool("is_active").
			Default(true),
		field.Time("starts_at").
			Default(time.Now),
		field.Time("ends_at").
			Optional().
			Nillable().
			Comment("Stops showing after this; null runs until deactivated"),
		field.JSON("metadata", map[string]any{}).
			Optional(),
		field.String("created_by").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the Announcement.
func (Announcement) Indexes() []ent.Index {
	return []ent.Index{
		// The active read filters on these two.
		index.Fields("is_active", "starts_at"),
	}
}
