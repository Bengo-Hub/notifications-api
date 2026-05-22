package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CreditTransaction holds the schema definition for credit transactions.
type CreditTransaction struct {
	ent.Schema
}

// Fields of the CreditTransaction.
func (CreditTransaction) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Tenant identifier"),
		field.Enum("type").
			Values("SMS", "WHATSAPP").
			Comment("Credit type (SMS or WHATSAPP)"),
		field.Enum("action").
			Values("TOPUP", "DEDUCTION", "REFUND", "ADJUSTMENT").
			Comment("Action performed"),
		field.Float("amount").
			Comment("Amount of credits moved"),
		field.Float("new_balance").
			Comment("Balance after transaction"),
		field.Float("provider_cost").
			Default(0).
			Comment("Actual provider cost for this transaction (e.g. Africa's Talking rate)"),
		field.Float("platform_fee").
			Default(0).
			Comment("Platform profit margin on this transaction (amount - provider_cost)"),
		field.String("reference_id").
			Optional().
			Comment("External reference (e.g. Treasury payment ID)"),
		field.Text("description").
			Optional().
			Comment("Transaction description"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the CreditTransaction.
func (CreditTransaction) Edges() []ent.Edge {
	return nil
}

// Indexes of the CreditTransaction.
func (CreditTransaction) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("type"),
		index.Fields("reference_id"),
	}
}
