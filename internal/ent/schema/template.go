package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Template holds the schema definition for a notification template registered in the DB.
// Actual content lives on the filesystem; this table stores metadata, tags, and variables.
type Template struct {
	ent.Schema
}

// Fields of the Template.
func (Template) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty().
			Comment("Template file stem, e.g. welcome, payment_receipt"),
		field.String("channel").
			NotEmpty().
			Comment("email, sms, push, whatsapp"),
		field.String("category").
			NotEmpty().
			Default("general").
			Comment("Folder category, e.g. auth, finance, logistics, shared"),
		field.JSON("tags", []string{}).
			Optional().
			Comment("Path-derived tag segments between channel and filename"),
		field.String("file_path").
			NotEmpty().
			Comment("Relative path from templates root, e.g. email/auth/welcome.html"),
		field.String("description").
			Default("").
			Optional().
			Comment("Optional human-readable description"),
		field.JSON("variables", []string{}).
			Optional().
			Comment("Extracted {{ .var }} names from template content"),
		field.String("mime_type").
			Default("text/plain").
			Comment("text/html, text/plain, application/json"),
		field.Bool("is_active").
			Default(true),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the Template.
func (Template) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("channel", "name").
			Unique().
			Annotations(entsql.IndexWhere("is_active = true")),
		index.Fields("channel", "category"),
		index.Fields("is_active"),
	}
}
