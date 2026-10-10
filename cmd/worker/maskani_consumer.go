package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/moneyfmt"
)

// Maskani (property platform) notifications. maskani-api publishes maskani.<event_type> through its
// outbox. Each mapped event goes out by email (when the payload has an address) and by WhatsApp
// (approved UTILITY template; any link is the template's URL button on maskaniapp, never body text).
// Customer messages go to the owner, occupant, buyer or visitor in the payload; staff alerts go to
// the tenant contact email and phone. Notices to an audience are sent by maskani-api through the
// send API, not from events.

// maskaniButtonBase is the domain fixed in every maskani_*_btn template (docs/whatsapp-template-policy.md).
const maskaniButtonBase = "https://maskaniapp.codevertexafrica.com"

// maskaniTZ renders times for East African estates (UTC+3, no daylight saving).
var maskaniTZ = time.FixedZone("EAT", 3*3600)

type maskaniMapping struct {
	TemplateID string // templates/{email,whatsapp}/<TemplateID>
	Subject    func(p map[string]any) string
	Staff      bool   // true: tenant contacts; false: the payload's own recipient
	PhoneKey   string // payload key of the customer's phone
	EmailKey   string // payload key of the customer's email
	// Path is the page under /{slug}/ the button and email link open ("" for none).
	Path func(p map[string]any) string
	// Data builds the template data shared by both channels.
	Data func(p map[string]any, ti *tenantInfo) map[string]any
	// WhatsApp template name and its body parameters, in {{1}}..{{n}} order.
	WATemplate string
	WAParams   func(d map[string]any) []string
	// WAVariant, when set, picks the template and parameters from the data instead, for messages
	// whose optional lines (such as VAT) need a different approved template rather than a blank.
	WAVariant func(d map[string]any) (string, []string)
	Skip      func(p map[string]any) bool
	// Responders: a staff alert goes to each of the payload's "responders" (the property staff
	// maskani-api picked) instead of the tenant contact, which stays the fallback.
	Responders bool
}

// maskaniResponder is one entry of a payload's "responders".
type maskaniResponder struct {
	UserID string
	Name   string
	Email  string
	Phone  string
}

func maskaniResponders(p map[string]any) []maskaniResponder {
	list, _ := p["responders"].([]any)
	out := make([]maskaniResponder, 0, len(list))
	for _, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, maskaniResponder{UserID: mStr(m, "user_id"), Name: mStr(m, "name"), Email: mStr(m, "email"), Phone: mStr(m, "phone")})
	}
	return out
}

func mStr(p map[string]any, key string) string {
	switch v := p[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// mMoney formats a decimal string amount ("6450.00") as "KES 6,450".
func mMoney(p map[string]any, key string) string {
	f, err := strconv.ParseFloat(mStr(p, key), 64)
	if err != nil {
		return mStr(p, key)
	}
	return moneyfmt.Format(f, "KES")
}

// mTime renders an RFC3339 time as "11 Oct 14:32" in East Africa Time.
func mTime(p map[string]any, key string) string {
	s := mStr(p, key)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.In(maskaniTZ).Format("2 Jan 15:04")
	}
	return s
}

// mMoneyOr formats key, or returns fallback when the payload has no such amount.
func mMoneyOr(p map[string]any, key, fallback string) string {
	if mStr(p, key) == "" {
		return fallback
	}
	return mMoney(p, key)
}

func mPositive(p map[string]any, key string) bool {
	f, err := strconv.ParseFloat(mStr(p, key), 64)
	return err == nil && f > 0
}

// maskaniPeriod turns "2026-11" into "November 2026".
func maskaniPeriod(ym string) string {
	if t, err := time.Parse("2006-01", ym); err == nil {
		return t.Format("January 2006")
	}
	return ym
}

// maskaniBillItem is one charge on a bill, formatted for the email table.
type maskaniBillItem struct {
	Description string
	Detail      string // "9 x KES 120" for metered or rated charges
	Amount      string
	Tax         string
}

// maxChargesParam keeps the one-line WhatsApp breakdown well inside Meta's 1,024 character body.
const maxChargesParam = 600

// maskaniBillItems formats bill.issued items for the email table and as one line for WhatsApp
// ("Service charge KES 4,500; Water 9 x KES 120 = KES 1,080"). Meta rejects newlines inside a
// template parameter, so the WhatsApp breakdown is a single line, cut short with "and N more" when
// a bill has many charges.
func maskaniBillItems(raw any) ([]maskaniBillItem, string) {
	list, _ := raw.([]any)
	items := make([]maskaniBillItem, 0, len(list))
	parts := make([]string, 0, len(list))
	for _, r := range list {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		it := maskaniBillItem{Description: waParam(m["description"], "Charge"), Amount: mMoney(m, "amount")}
		if mStr(m, "quantity") != "" && mStr(m, "rate") != "" {
			it.Detail = mStr(m, "quantity") + " x " + mMoney(m, "rate")
		}
		if mPositive(m, "tax") {
			it.Tax = mMoney(m, "tax")
		}
		items = append(items, it)
		part := it.Description + " " + it.Amount
		if it.Detail != "" {
			part = it.Description + " " + it.Detail + " = " + it.Amount
		}
		parts = append(parts, part)
	}
	line := ""
	for i, part := range parts {
		next := line
		if next != "" {
			next += "; "
		}
		next += part
		if len(next) > maxChargesParam {
			line += fmt.Sprintf("; and %d more", len(parts)-i)
			break
		}
		line = next
	}
	return items, line
}

func mParams(d map[string]any, keys ...string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = waParam(d[k], "-")
	}
	return out
}

func fixed(path string) func(map[string]any) string {
	return func(map[string]any) string { return path }
}

// withID links to prefix/<payload[key]>, or nowhere when the id is missing (never a broken link).
func withID(prefix, key string) func(map[string]any) string {
	return func(p map[string]any) string {
		if id := mStr(p, key); id != "" {
			return prefix + "/" + id
		}
		return ""
	}
}

var maskaniMappings = map[string]maskaniMapping{
	"bill.issued": {
		TemplateID: "maskani/bill_issued", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal"),
		Subject: func(p map[string]any) string {
			return fmt.Sprintf("Your %s bill for %s", mStr(p, "period"), mStr(p, "account_ref"))
		},
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			items, charges := maskaniBillItems(p["items"])
			total := mMoney(p, "amount")
			if charges == "" {
				charges = "see your statement"
			}
			return map[string]any{"name": waParam(p["name"], "there"), "period": maskaniPeriod(mStr(p, "period")),
				"account_ref": mStr(p, "account_ref"), "unit_code": mStr(p, "unit_code"), "amount": total,
				"subtotal": waParam(mMoneyOr(p, "subtotal", total), total), "tax_total": mMoney(p, "tax_total"),
				"has_tax": mPositive(p, "tax_total"), "items": items, "charges": charges,
				"invoice_date": mStr(p, "invoice_date"), "fund_name": mStr(p, "fund_name"),
				"due_date": mStr(p, "due_date"), "paybill": mStr(p, "paybill"), "invoice_number": mStr(p, "invoice_number"),
				"pay_account": waParam(p["pay_account"], mStr(p, "account_ref")), "pay_reference": mStr(p, "pay_reference")}
		},
		// Subtotal and VAT lines only on a taxed bill; a bill without a paybill would leave the pay
		// line empty, so it falls back to the portal button alone.
		WAVariant: func(d map[string]any) (string, []string) {
			if d["paybill"] == "" {
				return "maskani_bill_issued_nopaybill_v1_btn", mParams(d, "name", "period", "account_ref", "charges", "amount", "due_date")
			}
			if d["has_tax"] == true {
				return "maskani_bill_issued_vat_v1_btn",
					mParams(d, "name", "period", "account_ref", "charges", "subtotal", "tax_total", "amount", "due_date", "paybill", "pay_account")
			}
			return "maskani_bill_issued_v1_btn", mParams(d, "name", "period", "account_ref", "charges", "amount", "due_date", "paybill", "pay_account")
		},
	},
	"payment.applied": {
		TemplateID: "maskani/payment_received", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal"),
		Subject: func(p map[string]any) string { return "Payment received for " + mStr(p, "account_ref") },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["name"], "there"), "amount": mMoney(p, "amount"),
				"account_ref": mStr(p, "account_ref"), "receipt": mStr(p, "receipt"), "balance": mMoney(p, "balance"),
				"method": mStr(p, "method")}
		},
		WATemplate: "maskani_payment_received_v1_btn",
		WAParams: func(d map[string]any) []string {
			return mParams(d, "name", "amount", "account_ref", "receipt", "balance")
		},
	},
	"pass.created": {
		TemplateID: "maskani/visitor_pass", PhoneKey: "visitor_phone", EmailKey: "visitor_email",
		Subject: func(p map[string]any) string { return "Your gate code" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"visitor_name": waParam(p["visitor_name"], "there"), "estate": ti.Name,
				"code": mStr(p, "code"), "valid_from": mTime(p, "valid_from"), "valid_to": mTime(p, "valid_to")}
		},
		WATemplate: "maskani_visitor_pass_v1",
		WAParams: func(d map[string]any) []string {
			return mParams(d, "visitor_name", "estate", "code", "valid_from", "valid_to")
		},
		// A pass without a code (QR only) has nothing to send.
		Skip: func(p map[string]any) bool { return mStr(p, "code") == "" },
	},
	"visitor.arrived": {
		TemplateID: "maskani/visitor_arrived", PhoneKey: "host_phone", EmailKey: "host_email",
		Subject: func(p map[string]any) string { return mStr(p, "visitor_name") + " has arrived at the gate" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"host_name": waParam(p["host_name"], "there"), "visitor_name": waParam(p["visitor_name"], "Your visitor"),
				"unit_code": mStr(p, "unit_code"), "time": mTime(p, "occurred_at"), "vehicle_plate": mStr(p, "vehicle_plate")}
		},
		WATemplate: "maskani_visitor_arrived_v1",
		WAParams:   func(d map[string]any) []string { return mParams(d, "host_name", "visitor_name", "unit_code", "time") },
	},
	"walk_in.requested": {
		TemplateID: "maskani/walk_in_request", PhoneKey: "host_phone", EmailKey: "host_email",
		Path:    withID("portal/walk-ins", "event_id"),
		Subject: func(p map[string]any) string { return "Visitor at the gate for " + mStr(p, "unit_code") },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"host_name": waParam(p["host_name"], "there"), "visitor_name": waParam(p["visitor_name"], "A visitor"),
				"unit_code": mStr(p, "unit_code")}
		},
		WATemplate: "maskani_walk_in_request_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "host_name", "visitor_name", "unit_code") },
	},
	"party.invited": {
		TemplateID: "maskani/portal_invite", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal"),
		Subject: func(p map[string]any) string { return "Your owner portal is ready" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["name"], "there"), "estate": ti.Name}
		},
		WATemplate: "maskani_portal_invite_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "estate") },
	},
	"sale_contract.activated": {
		TemplateID: "maskani/contract_activated", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal/purchase"),
		Subject: func(p map[string]any) string { return "Sale agreement " + mStr(p, "contract_number") + " is active" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["buyer"], "there"), "contract_number": mStr(p, "contract_number"),
				"unit_code": mStr(p, "unit_code"), "account_ref": mStr(p, "account_ref"), "net_price": mMoney(p, "net_price")}
		},
		WATemplate: "maskani_contract_activated_v1_btn",
		WAParams: func(d map[string]any) []string {
			return mParams(d, "name", "contract_number", "unit_code", "account_ref")
		},
	},
	"sale_contract.fully_paid": {
		TemplateID: "maskani/contract_fully_paid", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal/purchase"),
		Subject: func(p map[string]any) string {
			return "Sale agreement " + mStr(p, "contract_number") + " is fully paid"
		},
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["name"], "there"), "contract_number": mStr(p, "contract_number"),
				"account_ref": mStr(p, "account_ref")}
		},
		WATemplate: "maskani_contract_fully_paid_v1",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "contract_number", "account_ref") },
	},
	"incident.reported": {
		TemplateID: "maskani/incident_alert", Staff: true,
		Path:    withID("security/incidents", "incident_id"),
		Subject: func(p map[string]any) string { return "Security alert: " + mStr(p, "title") },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "severity": mStr(p, "severity"), "title": mStr(p, "title"),
				"number": mStr(p, "number"), "category": mStr(p, "category")}
		},
		WATemplate: "maskani_incident_alert_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "estate", "severity", "title", "number") },
		Skip:       func(p map[string]any) bool { urgent, _ := p["urgent"].(bool); return !urgent },
	},
	// A resident's request: to the property's caretaker and manager (or the estate contact).
	"work_order.created": {
		TemplateID: "maskani/resident_request", Staff: true, Responders: true,
		Path:    withID("works", "work_order_id"),
		Subject: func(p map[string]any) string { return "New request from " + waParam(p["unit_code"], "a resident") + ": " + mStr(p, "title") },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "number": mStr(p, "number"), "priority": mStr(p, "priority"),
				"title": mStr(p, "title"), "unit_code": waParam(p["unit_code"], "a resident"), "category": mStr(p, "category"),
				"description": mStr(p, "description"), "requested_by": mStr(p, "requested_by")}
		},
		WATemplate: "maskani_resident_request_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "estate", "number", "unit_code", "priority", "title") },
		Skip:       func(p map[string]any) bool { return mStr(p, "source") != "resident" },
	},
	// Billing schedule: meters still unread before (or on) the billing day.
	"billing.readings_missing": {
		TemplateID: "maskani/readings_missing", Staff: true, Responders: true,
		Path: func(p map[string]any) string {
			return "utilities/readings?property_id=" + mStr(p, "property_id") + "&period=" + mStr(p, "period")
		},
		Subject: func(p map[string]any) string {
			return mStr(p, "missing_count") + " meter readings needed at " + mStr(p, "property") + " for " + maskaniPeriod(mStr(p, "period"))
		},
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			count, _ := strconv.Atoi(mStr(p, "missing_count"))
			more := count - 10
			if more < 0 {
				more = 0
			}
			return map[string]any{"estate": ti.Name, "property": mStr(p, "property"), "period": maskaniPeriod(mStr(p, "period")),
				"stage": mStr(p, "stage"), "billing_date": mStr(p, "billing_date"), "missing_count": mStr(p, "missing_count"),
				"missing_units": mStr(p, "missing_units"), "missing_more": more}
		},
		WATemplate: "maskani_readings_missing_v1_btn",
		WAParams: func(d map[string]any) []string {
			return mParams(d, "estate", "missing_count", "property", "period", "missing_units", "billing_date")
		},
	},
	// Billing schedule in remind mode: the run is ready for finance to start.
	"billing.run_ready": {
		TemplateID: "maskani/run_ready", Staff: true, Responders: true,
		Path:    func(p map[string]any) string { return "billing/runs?property_id=" + mStr(p, "property_id") },
		Subject: func(p map[string]any) string { return "Bills ready to run: " + mStr(p, "property") + ", " + maskaniPeriod(mStr(p, "period")) },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "property": mStr(p, "property"), "period": maskaniPeriod(mStr(p, "period")),
				"billing_date": mStr(p, "billing_date")}
		},
		WATemplate: "maskani_billing_ready_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "estate", "period", "property", "billing_date") },
	},
	// Collections ladder: a reminder to the owner with the paybill and account to pay to.
	"arrears.reminder": {
		TemplateID: "maskani/arrears_reminder", PhoneKey: "phone", EmailKey: "email",
		Path:    withID("portal/statement", "account_id"),
		Subject: func(p map[string]any) string { return "Payment reminder for account " + mStr(p, "account_ref") },
		Data:    arrearsData,
		WAVariant: func(d map[string]any) (string, []string) {
			if d["paybill"] == "" {
				return "maskani_arrears_reminder_nopaybill_v1_btn", mParams(d, "name", "account_ref", "estate", "balance", "days_overdue")
			}
			return "maskani_arrears_reminder_v1_btn", mParams(d, "name", "account_ref", "estate", "balance", "days_overdue", "paybill", "account_ref_pay")
		},
	},
	// Collections ladder: the demand letter, opened from the portal's documents.
	"arrears.demand_letter": {
		TemplateID: "maskani/demand_letter", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal/documents"),
		Subject: func(p map[string]any) string { return "Demand letter " + mStr(p, "document_number") + " for account " + mStr(p, "account_ref") },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			d := arrearsData(p, ti)
			d["document_number"] = mStr(p, "document_number")
			return d
		},
		WATemplate: "maskani_demand_letter_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "document_number", "account_ref", "estate", "balance") },
	},
	// Collections ladder: the last step, to finance and the property manager.
	"arrears.escalated": {
		TemplateID: "maskani/arrears_escalated", Staff: true, Responders: true,
		Path:    withID("billing/accounts", "account_id"),
		Subject: func(p map[string]any) string { return "Escalation: account " + mStr(p, "account_ref") + " is " + mStr(p, "days_overdue") + " days past due" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			d := arrearsData(p, ti)
			d["owner"], d["name"] = waParam(p["name"], "the owner"), ""
			return d
		},
		WATemplate: "maskani_arrears_escalated_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "estate", "account_ref", "unit_code", "balance", "days_overdue") },
	},
	// Instalment reminders: 3 days before, on the day, 7 and 14 days late.
	"instalment.due": {
		TemplateID: "maskani/instalment_due", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal/purchase"),
		Subject: func(p map[string]any) string {
			return "Instalment " + mStr(p, "seq") + " of " + mStr(p, "contract_number") + " is " + instalmentWhen(p)
		},
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "name": waParam(p["name"], "there"), "seq": mStr(p, "seq"),
				"contract_number": mStr(p, "contract_number"), "outstanding": mMoney(p, "outstanding"), "when": instalmentWhen(p),
				"due_date": mStr(p, "due_date"), "account_ref": waParam(p["pay_account"], mStr(p, "account_ref")), "paybill": mStr(p, "paybill"), "pay_reference": mStr(p, "pay_reference")}
		},
		WATemplate: "maskani_instalment_due_v1_btn",
		WAParams: func(d map[string]any) []string {
			return mParams(d, "name", "seq", "contract_number", "estate", "outstanding", "when", "account_ref")
		},
	},
	// A sale contract in default after its grace days, to sales, finance and the manager.
	"sale_contract.defaulted": {
		TemplateID: "maskani/contract_defaulted", Staff: true, Responders: true,
		Path:    withID("sales/contracts", "contract_id"),
		Subject: func(p map[string]any) string { return "Contract " + mStr(p, "contract_number") + " is in default" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "contract_number": mStr(p, "contract_number"),
				"oldest_due": mStr(p, "oldest_due"), "grace_days": mStr(p, "grace_days")}
		},
		WATemplate: "maskani_contract_defaulted_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "estate", "contract_number", "oldest_due", "grace_days") },
	},
	// A bank, cash, cheque or typed M-Pesa payment waiting for a reviewer (email and push only).
	"manual_payment.submitted": {
		TemplateID: "maskani/manual_payment", Staff: true, Responders: true,
		Path:    fixed("collections?tab=verify"),
		Subject: func(p map[string]any) string { return "Payment to verify: " + mStr(p, "account_ref") + ", " + mMoney(p, "amount") },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			methods := map[string]string{"bank_transfer": "bank transfer", "cash": "cash", "cheque": "cheque", "mpesa": "M-Pesa code"}
			return map[string]any{"estate": ti.Name, "account_ref": mStr(p, "account_ref"), "unit_code": waParam(p["unit_code"], "-"),
				"amount": mMoney(p, "amount"), "method": waParam(methods[mStr(p, "method")], mStr(p, "method")),
				"reference": mStr(p, "reference"), "submitted_by": waParam(p["submitted_by"], "a resident")}
		},
	},
	// A credit note or waiver waiting for approval (email and push only).
	"adjustment.requested": {
		TemplateID: "maskani/adjustment_requested", Staff: true, Responders: true,
		Path:    fixed("collections?tab=credits"),
		Subject: func(p map[string]any) string { return "Credit to approve: " + mStr(p, "account_ref") + ", " + mMoney(p, "amount") },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			kinds := map[string]string{"credit_note": "Credit note", "waiver": "Waiver"}
			return map[string]any{"estate": ti.Name, "account_ref": mStr(p, "account_ref"), "unit_code": waParam(p["unit_code"], "-"),
				"amount": mMoney(p, "amount"), "kind": waParam(kinds[mStr(p, "kind")], "Credit"), "reason": mStr(p, "reason"),
				"invoice_number": mStr(p, "invoice_number"), "requested_by": waParam(p["requested_by"], "finance")}
		},
	},
	// A resident queried a bill: finance and the manager hear (email and push only).
	"bill_query.raised": {
		TemplateID: "maskani/bill_query_raised", Staff: true, Responders: true,
		Path:    fixed("collections?tab=queries"),
		Subject: func(p map[string]any) string { return "Bill query from " + mStr(p, "account_ref") + ": " + mStr(p, "subject") },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "account_ref": mStr(p, "account_ref"), "unit_code": waParam(p["unit_code"], "-"),
				"subject": mStr(p, "subject"), "invoice_number": mStr(p, "invoice_number"), "raised_by": waParam(p["raised_by"], "a resident"),
				"due_by": mStr(p, "due_by")}
		},
	},
	// The answer to a resident's bill query, by email (the portal shows it too).
	"bill_query.answered": {
		TemplateID: "maskani/bill_query_answered", EmailKey: "email", Path: fixed("portal"),
		Subject: func(p map[string]any) string { return "Your bill query: " + mStr(p, "subject") },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "name": waParam(p["name"], "there"), "account_ref": mStr(p, "account_ref"),
				"subject": mStr(p, "subject"), "resolved": mStr(p, "status") == "resolved", "resolution": mStr(p, "resolution"),
				"invoice_number": mStr(p, "invoice_number")}
		},
	},
	"work_order.sla_breached": {
		TemplateID: "maskani/work_order_sla_breached", Staff: true,
		Path:    withID("works", "work_order_id"),
		Subject: func(p map[string]any) string { return "Work order " + mStr(p, "number") + " is past its SLA" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "number": mStr(p, "number"), "priority": mStr(p, "priority"),
				"title": mStr(p, "title"), "due_at": mTime(p, "resolution_due_at")}
		},
		WATemplate: "maskani_work_order_overdue_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "estate", "number", "priority", "title") },
	},
	// Email only: a renewal reminder is not urgent enough for WhatsApp.
	"vendor.document_expiring": {
		TemplateID: "maskani/vendor_document_expiring", Staff: true,
		Path:    withID("vendors", "vendor_id"),
		Subject: func(p map[string]any) string { return mStr(p, "vendor") + ": " + mStr(p, "doc_type") + " expires soon" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			expires := mStr(p, "expires_at")
			if t, err := time.Parse(time.RFC3339Nano, expires); err == nil {
				expires = t.In(maskaniTZ).Format("2 Jan 2006")
			}
			return map[string]any{"estate": ti.Name, "vendor": mStr(p, "vendor"), "doc_type": mStr(p, "doc_type"),
				"days_left": mStr(p, "days_left"), "expires_at": expires}
		},
	},
}

// startMaskaniConsumer subscribes to maskani.> and sends each mapped event by email and WhatsApp.
func startMaskaniConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, client *ent.Client, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping maskani consumer: NATS not available")
		return
	}
	log := logg.Named("maskani")

	handler := func(m *nats.Msg) {
		var evt eventslib.Event
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			log.Error("maskani event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}
		mp, ok := maskaniMappings[evt.EventType]
		if !ok || (mp.Skip != nil && mp.Skip(evt.Payload)) {
			_ = m.Ack()
			return
		}
		if evt.TenantID == uuid.Nil {
			_ = m.Ack()
			return
		}
		tenantID := evt.TenantID.String()
		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			log.Error("maskani event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		if err := dispatchMaskani(ctx, nc, cfg, ti, tenantID, evt, mp); err != nil {
			log.Error("maskani event: dispatch failed", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}
		// Best effort on top of WhatsApp and email: the host's own devices.
		pushMaskaniHost(ctx, nc, cfg, client, ti, evt, log)
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "maskani", "maskani.>", "notifications-maskani", handler,
		nats.BindStream("maskani"),
		nats.Durable("notifications-maskani"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}

// dispatchMaskani queues the email and WhatsApp messages for one event. Idempotency keys are per
// event and channel: several payments or passes share one aggregate, so the aggregate id would drop
// all but the first, while a redelivered event never sends twice.
func dispatchMaskani(ctx context.Context, nc *nats.Conn, cfg *config.Config, ti *tenantInfo, tenantID string, evt eventslib.Event, mp maskaniMapping) error {
	p := evt.Payload
	data := mp.Data(p, ti)
	suffix := ""
	if mp.Path != nil {
		if path := strings.Trim(mp.Path(p), "/"); path != "" && ti.Slug != "" {
			suffix = ti.Slug + "/" + path
			app := ti.ServiceURL("maskani", "NOTIFICATIONS_MASKANI_APP_URL", maskaniButtonBase)
			data["action_link"] = strings.TrimRight(app, "/") + "/" + suffix
		}
	}
	target := messaging.TargetCustomer
	email, phone := mStr(p, mp.EmailKey), mStr(p, mp.PhoneKey)
	if mp.Staff {
		target, email, phone = messaging.TargetStaff, ti.ContactEmail, ti.ContactPhone
	}
	if mp.Responders {
		if rs := maskaniResponders(p); len(rs) > 0 {
			for _, r := range rs {
				d := make(map[string]any, len(data)+1)
				for k, v := range data {
					d[k] = v
				}
				d["name"] = r.Name
				if err := deliverMaskani(ctx, nc, cfg, ti, tenantID, evt, mp, target, d, suffix, r.Email, r.Phone, "-"+r.UserID); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return deliverMaskani(ctx, nc, cfg, ti, tenantID, evt, mp, target, data, suffix, email, phone, "")
}

// deliverMaskani queues one recipient's email and WhatsApp. keySuffix tells recipients of the same
// event apart in the idempotency key.
func deliverMaskani(ctx context.Context, nc *nats.Conn, cfg *config.Config, ti *tenantInfo, tenantID string, evt eventslib.Event,
	mp maskaniMapping, target string, data map[string]any, suffix, email, phone, keySuffix string) error {
	p := evt.Payload
	send := func(channel, to string, meta map[string]any) error {
		meta["service_id"] = "maskani"
		_, err := messaging.Publish(ctx, nc, cfg.Events, messaging.Message{
			TenantID:       tenantID,
			Channel:        channel,
			TemplateID:     mp.TemplateID,
			SenderScope:    messaging.SenderScopeTenant,
			Target:         target,
			To:             []string{to},
			Data:           data,
			Metadata:       meta,
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("maskani-%s-%s%s", evt.ID, channel, keySuffix),
			QueuedAt:       time.Now(),
		})
		return err
	}

	if email != "" && strings.Contains(email, "@") && !strings.HasSuffix(strings.ToLower(email), ".local") {
		if err := send("email", email, map[string]any{"subject": mp.Subject(p)}); err != nil {
			return err
		}
	}
	waName, waParams := mp.WATemplate, []string(nil)
	if mp.WAVariant != nil {
		waName, waParams = mp.WAVariant(data)
	} else if mp.WAParams != nil {
		waParams = mp.WAParams(data)
	}
	if phone != "" && waName != "" {
		meta := map[string]any{
			"template_name":     waName,
			"template_language": "en_US",
			"template_params":   waParams,
		}
		if strings.HasSuffix(waName, "_btn") {
			if suffix == "" {
				return nil // a button template cannot go without its link
			}
			meta["template_button_param"] = suffix
		}
		if code := dialCodeForCountry(ti.Country); code != "" {
			meta["default_dial_code"] = code
		} else {
			meta["default_dial_code"] = "254"
		}
		if err := send("whatsapp", phone, meta); err != nil {
			return err
		}
	}
	return nil
}

// arrearsData is the template data shared by the collections ladder messages.
func arrearsData(p map[string]any, ti *tenantInfo) map[string]any {
	return map[string]any{"estate": ti.Name, "name": waParam(p["name"], "there"), "account_ref": mStr(p, "account_ref"),
		"account_ref_pay": waParam(p["pay_account"], mStr(p, "account_ref")), "pay_reference": mStr(p, "pay_reference"), "balance": mMoney(p, "balance"), "days_overdue": mStr(p, "days_overdue"),
		"paybill": mStr(p, "paybill"), "unit_code": waParam(p["unit_code"], "-"), "property": mStr(p, "property")}
}

// instalmentWhen says when an instalment falls due relative to today, in words.
func instalmentWhen(p map[string]any) string {
	days, _ := strconv.Atoi(mStr(p, "days_to_due"))
	switch {
	case days > 1:
		return "due in " + strconv.Itoa(days) + " days"
	case days == 1:
		return "due tomorrow"
	case days == 0:
		return "due today"
	case days == -1:
		return "1 day late"
	default:
		return strconv.Itoa(-days) + " days late"
	}
}
