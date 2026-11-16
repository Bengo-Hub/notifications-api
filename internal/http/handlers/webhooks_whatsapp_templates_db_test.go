package handlers

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/deliverylog"
	"github.com/bengobox/notifications-api/internal/testdb"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatecat"
)

// Meta's later verdict moves the delivery log row of a send: forward only (Meta can report "sent"
// after "delivered"), and a failure is final with Meta's code and reason. Runs on the local
// Postgres (testdb).
func TestRecordDeliveryOutcome(t *testing.T) {
	db := testdb.Open(t)
	ctx := context.Background()
	h := &WebhookHandler{client: db.Client, log: zap.NewNop()}
	row := func(id string) *ent.DeliveryLog {
		r, err := db.Client.DeliveryLog.Query().Where(deliverylog.ProviderMessageID(id)).Only(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, id := range []string{"wamid.ok", "wamid.bad"} {
		db.Client.DeliveryLog.Create().SetTenantID("t1").SetTemplateID("hadia/registry_published").
			SetChannel("whatsapp").SetRecipient("+254700000000").SetStatus("sent").SetProviderMessageID(id).SaveX(ctx)
	}

	h.recordDeliveryOutcome(ctx, templatecat.SendStatus{MessageID: "wamid.ok", Status: "delivered"})
	h.recordDeliveryOutcome(ctx, templatecat.SendStatus{MessageID: "wamid.ok", Status: "sent"})
	if got := row("wamid.ok").Status; got != "delivered" {
		t.Fatalf("after delivered then sent: %s", got)
	}
	h.recordDeliveryOutcome(ctx, templatecat.SendStatus{MessageID: "wamid.ok", Status: "read"})
	h.recordDeliveryOutcome(ctx, templatecat.SendStatus{MessageID: "wamid.ok", Status: "delivered"})
	if got := row("wamid.ok").Status; got != "read" {
		t.Fatalf("after read then delivered: %s", got)
	}

	h.recordDeliveryOutcome(ctx, templatecat.SendStatus{MessageID: "wamid.bad", Status: "failed", Code: 131049,
		Title: "This message was not delivered to maintain healthy ecosystem engagement."})
	bad := row("wamid.bad")
	if bad.Status != "failed" || bad.ErrorCode != "131049" || bad.ErrorDetail == "" {
		t.Fatalf("failed row: %+v", bad)
	}
}
