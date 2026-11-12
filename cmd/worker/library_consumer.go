package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/contacts"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// libraryEvent is the outbox envelope from library-api (shared-events).
type libraryEvent struct {
	ID            string                 `json:"id"`
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	TenantID      string                 `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

type libraryNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(data map[string]interface{}, tenantWebsite string) map[string]interface{}
}

// Keyed by {aggregate_type}.{event_type} — aggregate_type is always "library".
var libraryMappings = map[string]libraryNotificationMapping{
	"library.loan.overdue": {
		TemplateID:   "library/overdue_notice",
		EmailSubject: "Library item overdue",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":      "Librarian",
				"member_id": data["member_id"],
				"loan_id":   data["loan_id"],
				"due_at":    data["due_at"],
				"dashboard": fmt.Sprintf("%s/circulation", tenantWebsite),
			}
		},
	},
	"library.hold.ready": {
		TemplateID:   "library/hold_ready",
		EmailSubject: "A reserved item is ready for pickup",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":       "Librarian",
				"member_id":  data["member_id"],
				"bib_record": data["bib_record_id"],
				"expires_at": data["expires_at"],
				"dashboard":  fmt.Sprintf("%s/holds", tenantWebsite),
			}
		},
	},
	"library.fine.assessed": {
		TemplateID:   "library/fine_assessed",
		EmailSubject: "A fine was added to a member account",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				// member_name is filled from the resolved member (buildLibraryNotice); the raw
				// member id is never shown. reason arrives as a code (OVERDUE, membership).
				"name":        "Librarian",
				"member_name": "",
				"amount":      data["amount"],
				"reason":      fineReasonText(data["reason"]),
				"dashboard":   fmt.Sprintf("%s/fines", tenantWebsite),
			}
		},
	},
	"library.member.registered": {
		TemplateID:   "library/member_welcome",
		EmailSubject: "New library member registered",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":          "Librarian",
				"member_name":   data["display_name"],
				"membership_no": data["membership_no"],
				"dashboard":     fmt.Sprintf("%s/members", tenantWebsite),
			}
		},
	},
	"library.membership.fee_due": {
		TemplateID:   "library/membership_fee_due",
		EmailSubject: "Your library membership renewal is due",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":        "Member",
				"amount":      data["amount"],
				"expires_at":  formatEventDate(data["expires_at"]),
				"renewal_url": fmt.Sprintf("%s/my-account", tenantWebsite),
			}
		},
	},
	"library.ebook.expired": {
		TemplateID:   "library/ebook_expired",
		EmailSubject: "Your e-book loan has expired",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":    "Member",
				"title":   data["title"],
				"catalog": fmt.Sprintf("%s/ebooks", tenantWebsite),
			}
		},
	},
	"library.fine.paid": {
		TemplateID:   "library/fine_paid_receipt",
		EmailSubject: "Your library payment receipt",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":        "Member",
				"amount":      data["amount"],
				"description": data["description"],
				"dashboard":   fmt.Sprintf("%s/my-account", tenantWebsite),
			}
		},
	},
	"library.hold.expired": {
		TemplateID:   "library/hold_expired",
		EmailSubject: "Your hold pickup window has closed",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":      "Member",
				"bib_title": data["bib_title"],
				"dashboard": fmt.Sprintf("%s/my-account", tenantWebsite),
			}
		},
	},
	"library.loan.recalled": {
		TemplateID:   "library/loan_recalled",
		EmailSubject: "An item you have borrowed has been recalled",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":       "Member",
				"new_due_at": data["new_due_at"],
				"dashboard":  fmt.Sprintf("%s/my-account", tenantWebsite),
			}
		},
	},
	"library.serial.issue_late": {
		TemplateID:   "library/serial_issue_late",
		EmailSubject: "Serial issue overdue from vendor",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":            "Librarian",
				"subscription_id": data["subscription_id"],
				"issue_no":        data["issue_no"],
				"expected_date":   data["expected_date"],
				"dashboard":       fmt.Sprintf("%s/serials", tenantWebsite),
			}
		},
	},
}

// fineReasonText turns library-api's fine reason code into words for the email ("" when absent).
func fineReasonText(v interface{}) string {
	switch code, _ := v.(string); strings.ToUpper(code) {
	case "":
		return ""
	case "OVERDUE":
		return "Overdue loan"
	case "LOST":
		return "Lost item"
	case "DAMAGE":
		return "Damaged item"
	case "MEMBERSHIP":
		return "Membership fee"
	default:
		return strings.ToUpper(code[:1]) + strings.ToLower(strings.ReplaceAll(code[1:], "_", " "))
	}
}

// libraryMember is the member a library event is about. library-api sends crm_contact_id alone
// for a member linked to the CRM, and email and name only for an unlinked member; when both are
// present the CRM contact wins field by field. The error is set when the CRM could not answer.
func libraryMember(ctx context.Context, res *contacts.Resolver, evt libraryEvent, ti *tenantInfo) (contacts.Recipient, error) {
	email, _ := evt.Payload["email"].(string)
	name, _ := evt.Payload["name"].(string)
	fallback := contacts.Recipient{Name: name, Email: email}
	contactID, _ := evt.Payload["crm_contact_id"].(string)
	id := contacts.ParseID(contactID)
	if id == uuid.Nil {
		return fallback, nil
	}
	tenantID := contacts.ParseID(evt.TenantID)
	if ti != nil && ti.ID != uuid.Nil {
		tenantID = ti.ID
	}
	return res.Lookup(ctx, tenantID, id, fallback)
}

// libraryNotice is one library email: who it goes to, as whom, and the template data.
type libraryNotice struct {
	To     string
	Target string
	Data   map[string]interface{}
}

// noticeOutcome is what a consumer does with an event once the recipient is resolved.
type noticeOutcome int

const (
	noticeSend  noticeOutcome = iota // send (an empty To means nobody to send to: ack)
	noticeRetry                      // the CRM was unreachable: redeliver later
	noticeDrop                       // a linked member with no email: ack without sending
)

// libraryContactID is the CRM contact the event names, or uuid.Nil.
func libraryContactID(evt libraryEvent) uuid.UUID {
	raw, _ := evt.Payload["crm_contact_id"].(string)
	return contacts.ParseID(raw)
}

// buildLibraryNotice picks the recipient and template data for an event.
//   - The member gets it when they have an email.
//   - A member linked to the CRM (the event carries crm_contact_id) never falls back to the
//     librarian: with no email resolved the outcome is noticeRetry when the CRM could not be
//     reached, else noticeDrop (CRM client switched off, or the CRM does not know the contact).
//   - An unlinked member without an email goes to the tenant's librarian address, as before
//     (To is empty when there is none either).
func buildLibraryNotice(ctx context.Context, res *contacts.Resolver, evt libraryEvent, ti *tenantInfo, mapping libraryNotificationMapping) (libraryNotice, noticeOutcome) {
	member, lookupErr := libraryMember(ctx, res, evt, ti)
	if member.Email == "" && libraryContactID(evt) != uuid.Nil {
		if lookupErr != nil {
			return libraryNotice{}, noticeRetry
		}
		return libraryNotice{}, noticeDrop
	}
	n := libraryNotice{To: ti.ContactEmail, Target: messaging.TargetStaff}
	if member.Email != "" {
		n.To, n.Target = member.Email, messaging.TargetCustomer
	}
	n.Data = mapping.DataBuilder(evt.Payload, ti.Website)
	if member.Name != "" {
		n.Data["name"] = member.Name // greet the actual member when known
		if _, ok := n.Data["member_name"]; ok {
			n.Data["member_name"] = member.Name
		}
	}
	return n, noticeSend
}

// startLibraryConsumer subscribes to library.> events and republishes them as
// notification messages (routed to the tenant librarian / admin).
func startLibraryConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, crmContacts *contacts.Resolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping library consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt libraryEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("library event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}
		mapping, ok := libraryMappings[evt.AggregateType+"."+evt.EventType]
		if !ok {
			_ = m.Ack()
			return
		}
		ti, err := tr.resolve(ctx, evt.TenantID)
		if err != nil {
			logg.Error("library event: failed to resolve tenant", zap.String("tenant_id", evt.TenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		notice, outcome := buildLibraryNotice(ctx, crmContacts, evt, ti, mapping)
		switch outcome {
		case noticeRetry:
			retryCRMLookup(m, logg, "library", evt.EventType, evt.TenantID, libraryContactID(evt).String())
			return
		case noticeDrop:
			logg.Info("library event: linked member has no email in the CRM, notice dropped",
				zap.String("type", evt.EventType), zap.String("tenant_id", evt.TenantID),
				zap.String("crm_contact_id", libraryContactID(evt).String()))
			_ = m.Ack()
			return
		}
		// Patron-direct when the member has an email; else fall back to the librarian.
		recipient, target, data := notice.To, notice.Target, notice.Data
		if recipient == "" {
			_ = m.Ack()
			return
		}

		msg := messaging.Message{
			TenantID:    evt.TenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      target,
			To:          []string{recipient},
			Data:        data,
			Metadata: map[string]interface{}{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("library-%s-%s", evt.EventType, evt.ID),
			QueuedAt:       time.Now(),
		}
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("library event: failed to dispatch notification", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}
		logg.Info("library notification dispatched", zap.String("type", evt.EventType), zap.String("template", mapping.TemplateID))
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "library", "library.>", "notifications-library", handler,
		nats.BindStream("library"),
		nats.Durable("notifications-library"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(crmRetryMaxDeliver),
	)
}
