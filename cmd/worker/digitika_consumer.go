package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// digitikaEvent matches the platform-standard shared-events envelope published by
// codevertex-website's events.ts using the shared-events format:
//
//	subject: digitika.{event_type}  (e.g. digitika.enrollment.confirmed)
//	body:    { id, event_type, aggregate_type, aggregate_id, tenant_id, tenant_slug, payload, ... }
type digitikaEvent struct {
	ID            string         `json:"id"`
	EventType     string         `json:"event_type"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	TenantID      string         `json:"tenant_id"`   // slug: "codevertex"
	TenantSlug    string         `json:"tenant_slug"` // slug: "codevertex"
	Payload       map[string]any `json:"payload"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

var digitikaOrdinals = []string{"", "1st", "2nd", "3rd", "4th", "5th", "6th", "7th", "8th", "9th", "10th"}

func ordinalLabel(n int) string {
	if n >= 1 && n < len(digitikaOrdinals) {
		return digitikaOrdinals[n]
	}
	return fmt.Sprintf("%dth", n)
}

// digitikaNotice is one Digitika notification ready to send: its email template, the WhatsApp
// text file when that differs, the subject, the data every party shares and the parties.
type digitikaNotice struct {
	TemplateID   string // email template and preference key
	WATemplateID string // WhatsApp text file when it differs from TemplateID
	Subject      string
	Scope        string // messaging.SenderScope* (default tenant)
	Data         map[string]any
	Parties      []noticeParty
	// Ungated skips the tenant preference gate: a one-time code is never switched off, and its
	// WhatsApp text file (auth/otp) is not the email template the gate is keyed on.
	Ungated bool
}

// digitikaBuilders turn each website event into its notice. They are pure (payload in, notice
// out) so every wording and every recipient rule is tested without NATS. One consumer, one
// table: a new Digitika notification is one builder here, one email template and one WhatsApp
// template.
var digitikaBuilders = map[string]func(p map[string]any) (digitikaNotice, bool){
	"enrollment.confirmed":  buildEnrollmentConfirmed,
	"payment.succeeded":     buildInstallmentReceipt,
	"installment.reminder":  buildInstallmentReminder,
	"account.created":       buildPortalAccount,
	"access_code.requested": buildAccessCode,
	"contact.received":      buildContactReceived,
	"certificate.issued":    buildCertificateIssued,
}

func buildEnrollmentConfirmed(p map[string]any) (digitikaNotice, bool) {
	people := peopleFromPayload(p)
	course, currency := str(p, "courseName"), str(p, "currency")
	total := num(p, "totalAmount")
	link := str(p, "portalLink")
	button := digitikaButton(link)
	return digitikaNotice{
		TemplateID: "digitika/enrollment_confirmed",
		Subject:    "Enrollment confirmed: " + course,
		Data: map[string]any{
			"student_name":         people.StudentName,
			"course_name":          course,
			"payment_plan":         str(p, "paymentPlan"),
			"total_amount":         fmt.Sprintf("%.0f", total),
			"currency":             currency,
			"student_id":           people.StudentID,
			"cohort_name":          str(p, "cohortName"),
			"portal_link":          link,
			"installments_summary": str(p, "installmentsSummary"),
		},
		Parties: people.parties(messaging.TargetCustomer, func(r recipient) *partyWhatsApp {
			return &partyWhatsApp{
				Name:   "digitika_enrollment_confirmed_v1_btn",
				Params: []string{waParam(r.Name, "there"), r.Place(), waParam(course, "your course"), waParam(people.StudentID, "-"), amountText(total, currency)},
				Button: button,
			}
		}),
	}, hasContact(people)
}

func buildInstallmentReceipt(p map[string]any) (digitikaNotice, bool) {
	people := peopleFromPayload(p)
	course, currency := str(p, "courseName"), str(p, "currency")
	no, total := int(num(p, "installmentNo")), int(num(p, "totalInstallments"))
	paid, remaining := num(p, "amountPaid"), num(p, "remainingBalance")
	link := str(p, "portalLink")
	button := digitikaButton(link)

	n := digitikaNotice{
		TemplateID: "digitika/installment_receipt",
		Subject:    fmt.Sprintf("Payment received, %s installment: %s", ordinalLabel(no), course),
		Data: map[string]any{
			"student_name":            people.StudentName,
			"course_name":             course,
			"payment_label":           ordinalLabel(no),
			"amount_paid":             fmt.Sprintf("%.0f", paid),
			"currency":                currency,
			"payment_ref":             str(p, "paymentRef"),
			"remaining_balance":       blankZero(remaining),
			"next_installment_date":   str(p, "nextInstallmentDate"),
			"next_installment_amount": blankZero(num(p, "nextInstallmentAmount")),
			"student_id":              people.StudentID,
			"portal_link":             link,
		},
	}
	// A WhatsApp template cannot hide a line, so a payment that clears the balance has its own.
	paidInFull := remaining <= 0
	if paidInFull {
		n.WATemplateID = "digitika/payment_complete"
	}
	n.Parties = people.parties(messaging.TargetCustomer, func(r recipient) *partyWhatsApp {
		if paidInFull {
			return &partyWhatsApp{
				Name:   "digitika_payment_complete_v1_btn",
				Params: []string{waParam(r.Name, "there"), amountText(paid, currency), r.Whose(), waParam(course, "the course")},
				Button: button,
			}
		}
		return &partyWhatsApp{
			Name:   "digitika_installment_receipt_v1_btn",
			Params: []string{waParam(r.Name, "there"), amountText(paid, currency), r.Whose(), waParam(course, "the course"), ordinalOf(no, total), amountText(remaining, currency)},
			Button: button,
		}
	})
	return n, hasContact(people)
}

func buildInstallmentReminder(p map[string]any) (digitikaNotice, bool) {
	people := peopleFromPayload(p)
	course, currency := str(p, "courseName"), str(p, "currency")
	no, total := int(num(p, "installmentNo")), int(num(p, "totalInstallments"))
	amount := num(p, "amount")
	due := str(p, "dueDate")
	days := int(num(p, "daysUntilDue"))
	overdue := days < 0
	link := str(p, "portalLink")
	button := digitikaButton(link)

	n := digitikaNotice{
		TemplateID: "digitika/installment_reminder",
		Subject:    fmt.Sprintf("Payment reminder, %s installment due: %s", ordinalLabel(no), course),
		Data: map[string]any{
			"student_name":       people.StudentName,
			"course_name":        course,
			"payment_label":      ordinalLabel(no),
			"total_installments": total,
			"amount":             fmt.Sprintf("%.0f", amount),
			"currency":           currency,
			"due_date":           prettyDate(due),
			"days_until_due":     days,
			"overdue":            overdue,
			"days_overdue":       int(math.Abs(float64(days))),
			"student_id":         people.StudentID,
			"portal_link":        link,
		},
	}
	waName := "digitika_installment_reminder_v1_btn"
	if overdue {
		n.Subject = fmt.Sprintf("Payment overdue, %s installment: %s", ordinalLabel(no), course)
		n.WATemplateID = "digitika/installment_overdue"
		waName = "digitika_installment_overdue_v1_btn"
	}
	n.Parties = people.parties(messaging.TargetCustomer, func(r recipient) *partyWhatsApp {
		return &partyWhatsApp{
			Name:   waName,
			Params: []string{waParam(r.Name, "there"), ordinalOf(no, total), amountText(amount, currency), r.Whose(), waParam(course, "the course"), waParam(prettyDate(due), "the due date")},
			Button: button,
		}
	})
	return n, hasContact(people)
}

func buildPortalAccount(p map[string]any) (digitikaNotice, bool) {
	email, phone := strings.TrimSpace(str(p, "email")), strings.TrimSpace(str(p, "phone"))
	role, name, student := str(p, "accountRole"), str(p, "name"), str(p, "studentName")
	loginURL := str(p, "loginUrl")
	sentence := "Your Digitika student account is ready."
	subject := "Your Digitika student account"
	if role == "parent" {
		sentence = fmt.Sprintf("Your parent account for %s is ready on Digitika.", student)
		subject = fmt.Sprintf("Your parent account for %s at Digitika", student)
	}
	n := digitikaNotice{
		TemplateID: "digitika/portal_account",
		Subject:    subject,
		Data: map[string]any{
			"name":          name,
			"email":         email,
			"account_role":  role,
			"course_name":   str(p, "courseName"),
			"student_name":  student,
			"temp_password": str(p, "tempPassword"), // email only: a password is never sent on WhatsApp
			"login_url":     loginURL,
		},
	}
	// The sign-in details go by email; WhatsApp only says the account is ready and where to sign in.
	var wa *partyWhatsApp
	if email != "" {
		wa = &partyWhatsApp{
			Name:   "digitika_portal_account_v1_btn",
			Params: []string{waParam(name, "there"), sentence, email},
			Button: digitikaButton(loginURL),
		}
	}
	n.Parties = []noticeParty{partyOf(messaging.TargetCustomer, email, phone, nil, wa)}
	return n, email != "" || phone != ""
}

func buildAccessCode(p map[string]any) (digitikaNotice, bool) {
	email, phone := strings.TrimSpace(str(p, "email")), strings.TrimSpace(str(p, "phone"))
	code := str(p, "code")
	ttl := int(num(p, "ttlMinutes"))
	if ttl <= 0 {
		ttl = 10
	}
	n := digitikaNotice{
		TemplateID:   "auth/otp_verification",
		WATemplateID: "auth/otp",
		Subject:      "Your Digitika parent portal code",
		// A one-time code goes from the platform number, so a tenant's own WhatsApp plan and
		// quota never block it (the same rule as sign-in codes in auth_consumer.go).
		Scope:   messaging.SenderScopePlatform,
		Ungated: true,
		Data: map[string]any{
			"name": str(p, "name"), "otp": code, "ttl_minutes": ttl, "brand_name": "Digitika parent portal",
		},
	}
	var wa *partyWhatsApp
	if code != "" {
		wa = &partyWhatsApp{Name: "auth_otp", Params: []string{code}, Button: code}
	}
	n.Parties = []noticeParty{partyOf(messaging.TargetCustomer, email, phone, nil, wa)}
	return n, code != "" && (email != "" || phone != "")
}

func buildContactReceived(p map[string]any) (digitikaNotice, bool) {
	email, phone := strings.TrimSpace(str(p, "email")), strings.TrimSpace(str(p, "phone"))
	name := str(p, "name")
	n := digitikaNotice{
		TemplateID: "digitika/contact_form_reply",
		Subject:    "We received your message: Codevertex Africa Limited",
		Data: map[string]any{
			"name": name, "email": email, "message": str(p, "message"), "service": str(p, "service"),
		},
	}
	n.Parties = []noticeParty{partyOf(messaging.TargetCustomer, email, phone, nil,
		&partyWhatsApp{Name: "digitika_contact_received_v1", Params: []string{waParam(name, "there")}})}
	return n, email != "" || phone != ""
}

func buildCertificateIssued(p map[string]any) (digitikaNotice, bool) {
	people := peopleFromPayload(p)
	course, number := str(p, "courseName"), str(p, "certificateNumber")
	link := str(p, "certificateLink")
	button := digitikaButton(link)
	return digitikaNotice{
		TemplateID: "digitika/certificate_issued",
		Subject:    "Your certificate: " + course,
		Data: map[string]any{
			"student_name":       people.StudentName,
			"course_name":        course,
			"certificate_number": number,
			"certificate_link":   link,
		},
		Parties: people.parties(messaging.TargetCustomer, func(r recipient) *partyWhatsApp {
			return &partyWhatsApp{
				Name:   "digitika_certificate_issued_v1_btn",
				Params: []string{waParam(r.Name, "there"), r.Who(), waParam(course, "the course"), waParam(number, "-")},
				Button: button,
			}
		}),
	}, hasContact(people)
}

// hasContact reports whether anyone in the notice can be reached at all.
func hasContact(d digitikaPeople) bool {
	return d.StudentEmail != "" || d.StudentPhone != "" || d.GuardianEmail != "" || d.GuardianPhone != ""
}

// blankZero is "" for no amount, so a template's optional line stays hidden.
func blankZero(v float64) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf("%.0f", v)
}

// startDigitikaConsumer subscribes to digitika.> JetStream events published by
// codevertex-website and sends each as a party notice: the learner and, when there is one, the
// parent or guardian, on email and WhatsApp (feedback_active_channels_email_whatsapp), with the
// tenant's channel preferences applied.
func startDigitikaConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver,
	gate *preferences.Gate, templateChannels map[string][]string, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping digitika consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt digitikaEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("digitika event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}
		build, known := digitikaBuilders[evt.EventType]
		if !known {
			logg.Debug("digitika event: unhandled event_type, skipping", zap.String("event_type", evt.EventType))
			_ = m.Ack()
			return
		}
		if evt.Payload == nil {
			logg.Warn("digitika event: empty payload", zap.String("event_type", evt.EventType))
			_ = m.Ack()
			return
		}
		notice, reachable := build(evt.Payload)
		if !reachable {
			logg.Warn("digitika event: nobody to notify (no email or phone)", zap.String("event_type", evt.EventType))
			_ = m.Ack()
			return
		}

		// The event carries the tenant slug ("codevertex"), not its UUID.
		slug := evt.TenantSlug
		if slug == "" {
			slug = evt.TenantID
		}
		tenantID, country := slug, ""
		if ti, err := tr.resolveBySlug(ctx, slug); err == nil {
			tenantID, country = ti.ID.String(), ti.Country
		} else {
			logg.Warn("digitika event: tenant slug not resolved, using slug as fallback", zap.String("slug", slug), zap.Error(err))
		}

		pn := partyNotice{
			ServiceID:        "digitika",
			TenantID:         tenantID,
			TemplateID:       notice.TemplateID,
			WATemplateID:     notice.WATemplateID,
			Subject:          notice.Subject,
			Scope:            notice.Scope,
			Data:             notice.Data,
			Country:          country,
			IdemBase:         "digitika-" + evt.EventType + "-" + evt.ID,
			Parties:          notice.Parties,
			Gate:             gate,
			TemplateChannels: templateChannels,
		}
		if notice.Ungated {
			pn.Gate, pn.TemplateChannels = nil, nil
		}
		if err := sendToParties(ctx, nc, cfg, pn); err != nil {
			logg.Error("digitika event: failed to dispatch notification",
				zap.String("event_type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}
		logg.Info("digitika notification dispatched",
			zap.String("event_type", evt.EventType),
			zap.String("template", notice.TemplateID),
			zap.Int("parties", len(notice.Parties)),
		)
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "digitika", "digitika.>", "notifications-digitika", handler,
		nats.BindStream("digitika"),
		nats.Durable("notifications-digitika"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
