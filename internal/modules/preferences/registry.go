// Package preferences implements per-tenant notification gating: every notification type
// (messaging.Message.TemplateID) is classified and individually toggleable per tenant.
//
//   - LOCKED types are security/credential-critical (password reset, OTP, welcome-with-
//     credentials) — always delivered, never toggleable.
//   - ESSENTIAL types default ON when a tenant has no explicit setting (transactional
//     customer/business communications: receipts, invoices, dunning, order lifecycle).
//   - OPTIONAL types default OFF unless the tenant explicitly enables them in settings
//     (engagement, informational alerts, status pings).
//
// Effective value resolution: locked → tenant ServiceConfig override → platform
// ServiceConfig default → registry class default. Unregistered template ids FAIL OPEN
// (delivered) so a newly added template is never silently dropped before it is
// classified here — add new templates to this registry when introducing them.
package preferences

// Class is the gating classification of a notification type.
type Class string

const (
	// ClassLocked notifications are always delivered and cannot be disabled.
	ClassLocked Class = "locked"
	// ClassEssential notifications default ON (deliver unless tenant disabled them).
	ClassEssential Class = "essential"
	// ClassOptional notifications default OFF (deliver only when tenant enabled them).
	ClassOptional Class = "optional"
)

// Type describes one notification type (keyed by the worker Message.TemplateID).
type Type struct {
	Key   string `json:"key"`   // TemplateID, e.g. "finance/payment_success"
	Label string `json:"label"` // human label for the settings UI
	Group string `json:"group"` // settings-UI grouping
	Class Class  `json:"class"`
}

// ConfigKey returns the ServiceConfig key storing the toggle for a template id.
func ConfigKey(templateID string) string {
	return "notifications.type." + templateID + ".enabled"
}

// Registry is the authoritative classification of every notification type the worker
// dispatches. Grouped to mirror the consumers in cmd/worker.
var Registry = []Type{
	// ── Account & security (LOCKED — never gated) ────────────────────────────
	{Key: "auth/welcome", Label: "Welcome / account created", Group: "Account & Security", Class: ClassLocked},
	{Key: "auth/password_reset", Label: "Password reset", Group: "Account & Security", Class: ClassLocked},
	{Key: "auth/otp_verification", Label: "One-time passcodes (OTP)", Group: "Account & Security", Class: ClassLocked},

	// ── Billing & payments ───────────────────────────────────────────────────
	{Key: "finance/payment_success", Label: "Payment successful", Group: "Billing & Payments", Class: ClassEssential},
	{Key: "finance/payment_failed", Label: "Payment failed", Group: "Billing & Payments", Class: ClassEssential},
	{Key: "finance/payment_receipt", Label: "Payment receipt", Group: "Billing & Payments", Class: ClassEssential},
	{Key: "finance/refund_completed", Label: "Refund completed", Group: "Billing & Payments", Class: ClassEssential},
	{Key: "finance/invoice_sent", Label: "Invoice issued", Group: "Billing & Payments", Class: ClassEssential},
	{Key: "finance/invoice_overdue", Label: "Invoice overdue reminder", Group: "Billing & Payments", Class: ClassEssential},
	{Key: "finance/payout_completed", Label: "Payout completed", Group: "Billing & Payments", Class: ClassOptional},

	// ── Subscription lifecycle ───────────────────────────────────────────────
	{Key: "subscription/subscription_expiring", Label: "Subscription expiring", Group: "Subscription", Class: ClassEssential},
	{Key: "subscription/grace_reminder", Label: "Grace-period reminder", Group: "Subscription", Class: ClassEssential},
	{Key: "platform/plan_expiry_warning", Label: "Plan expiry warning", Group: "Subscription", Class: ClassEssential},
	{Key: "subscription/subscription_created", Label: "Subscription created", Group: "Subscription", Class: ClassOptional},
	{Key: "subscription/subscription_renewed", Label: "Subscription renewed", Group: "Subscription", Class: ClassOptional},
	{Key: "subscription/subscription_upgraded", Label: "Subscription upgraded", Group: "Subscription", Class: ClassOptional},
	{Key: "subscription/subscription_downgraded", Label: "Subscription downgraded", Group: "Subscription", Class: ClassOptional},
	{Key: "subscription/subscription_cancelled", Label: "Subscription cancelled", Group: "Subscription", Class: ClassOptional},

	// ── Orders (customer-facing lifecycle) ───────────────────────────────────
	{Key: "ordering/order_placed", Label: "Order placed confirmation", Group: "Orders", Class: ClassEssential},
	{Key: "ordering/order_ready", Label: "Order ready", Group: "Orders", Class: ClassEssential},
	{Key: "ordering/order_for_pickup", Label: "Order ready for pickup", Group: "Orders", Class: ClassEssential},
	{Key: "ordering/order_out_for_delivery", Label: "Order out for delivery", Group: "Orders", Class: ClassEssential},
	{Key: "ordering/order_delivered", Label: "Order delivered", Group: "Orders", Class: ClassEssential},
	{Key: "ordering/order_cancelled", Label: "Order cancelled", Group: "Orders", Class: ClassEssential},
	{Key: "ordering/order_refunded", Label: "Order refunded", Group: "Orders", Class: ClassEssential},
	{Key: "ordering/order_scheduled", Label: "Order scheduled", Group: "Orders", Class: ClassEssential},
	{Key: "ordering/new_order_tenant", Label: "New-order staff alert", Group: "Orders", Class: ClassEssential},

	// ── POS ──────────────────────────────────────────────────────────────────
	{Key: "pos/pos_payment_receipt", Label: "POS payment receipt", Group: "POS", Class: ClassEssential},
	{Key: "pos/pos_order_ready", Label: "POS order ready", Group: "POS", Class: ClassEssential},
	{Key: "pos/kds_waiter_called", Label: "Waiter called (KDS)", Group: "POS", Class: ClassEssential},
	{Key: "pos/return_completed", Label: "Return completed", Group: "POS", Class: ClassEssential},
	{Key: "pos/layaway_payment_due", Label: "Layaway payment due", Group: "POS", Class: ClassEssential},
	{Key: "pos/appointment_created", Label: "Appointment booked", Group: "POS", Class: ClassEssential},
	{Key: "pos/appointment_reminder", Label: "Appointment reminder", Group: "POS", Class: ClassOptional},
	{Key: "pos/hotel_check_in", Label: "Hotel check-in", Group: "POS", Class: ClassOptional},
	{Key: "pos/hotel_check_out", Label: "Hotel check-out", Group: "POS", Class: ClassOptional},
	{Key: "pos/loyalty_points_earned", Label: "Loyalty points earned", Group: "POS", Class: ClassOptional},
	{Key: "pos/loyalty_tier_upgraded", Label: "Loyalty tier upgraded", Group: "POS", Class: ClassOptional},
	{Key: "pos/alert_stock_low", Label: "POS stock-low alert", Group: "POS", Class: ClassOptional},

	// ── Events / ticketing ───────────────────────────────────────────────────
	{Key: "events/ticket_issued", Label: "Event ticket issued", Group: "Events", Class: ClassEssential},

	// ── Library ──────────────────────────────────────────────────────────────
	{Key: "library/member_welcome", Label: "Member welcome", Group: "Library", Class: ClassEssential},
	{Key: "library/overdue_notice", Label: "Overdue notice", Group: "Library", Class: ClassEssential},
	{Key: "library/hold_ready", Label: "Hold ready for pickup", Group: "Library", Class: ClassEssential},
	{Key: "library/fine_assessed", Label: "Fine assessed", Group: "Library", Class: ClassEssential},
	{Key: "library/fine_paid_receipt", Label: "Fine paid receipt", Group: "Library", Class: ClassEssential},
	{Key: "library/membership_fee_due", Label: "Membership fee due", Group: "Library", Class: ClassEssential},
	{Key: "library/ebook_expired", Label: "E-book loan expired", Group: "Library", Class: ClassOptional},
	{Key: "library/hold_expired", Label: "Hold expired", Group: "Library", Class: ClassOptional},
	{Key: "library/loan_recalled", Label: "Loan recalled", Group: "Library", Class: ClassOptional},
	{Key: "library/serial_issue_late", Label: "Serial issue late", Group: "Library", Class: ClassOptional},

	// ── Logistics / riders ───────────────────────────────────────────────────
	{Key: "logistics/rider_invite", Label: "Rider invite (credentials)", Group: "Logistics", Class: ClassEssential},
	{Key: "logistics/rider_onboarding_approved", Label: "Rider onboarding approved", Group: "Logistics", Class: ClassEssential},
	{Key: "logistics/delivery_assigned", Label: "Delivery assigned (rider)", Group: "Logistics", Class: ClassEssential},
	{Key: "logistics/delivery_failed", Label: "Delivery failed alert", Group: "Logistics", Class: ClassEssential},
	{Key: "logistics/delivery_en_route", Label: "Delivery en route", Group: "Logistics", Class: ClassOptional},
	{Key: "logistics/delivery_completed", Label: "Delivery completed", Group: "Logistics", Class: ClassOptional},
	{Key: "logistics/rider_kyc_submitted", Label: "Rider KYC submitted", Group: "Logistics", Class: ClassOptional},
	{Key: "logistics/rider_suspended", Label: "Rider suspended", Group: "Logistics", Class: ClassOptional},
	{Key: "logistics/rider_rejected", Label: "Rider rejected", Group: "Logistics", Class: ClassOptional},
	{Key: "logistics/rider_expired", Label: "Rider invite expired", Group: "Logistics", Class: ClassOptional},

	// ── Inventory alerts (also source-gated by the publishing service's own toggle) ──
	{Key: "inventory/low_stock_alert", Label: "Low stock alert", Group: "Inventory", Class: ClassOptional},
	{Key: "inventory/stock_out", Label: "Stock-out alert", Group: "Inventory", Class: ClassOptional},

	// ── Support / projects / education ───────────────────────────────────────
	{Key: "ticketing/ticket_assigned", Label: "Support ticket assigned", Group: "Support & Projects", Class: ClassOptional},
	{Key: "ticketing/ticket_resolved", Label: "Support ticket resolved", Group: "Support & Projects", Class: ClassOptional},
	{Key: "projects/project_milestone_reached", Label: "Project milestone reached", Group: "Support & Projects", Class: ClassOptional},
	{Key: "digitika/enrollment_confirmed", Label: "Enrollment confirmed", Group: "Education", Class: ClassEssential},
	{Key: "digitika/installment_receipt", Label: "Installment receipt", Group: "Education", Class: ClassEssential},

	// ── Generic passthrough (ERP payslips/vouchers/HR mail and other raw branded
	//    sends route through this shared template) ─────────────────────────────
	{Key: "shared/generic_notification", Label: "Business emails (payslips, vouchers, HR & raw sends)", Group: "General", Class: ClassEssential},
}

// registryByKey indexes Registry for lookups.
var registryByKey = func() map[string]Type {
	m := make(map[string]Type, len(Registry))
	for _, t := range Registry {
		m[t.Key] = t
	}
	return m
}()

// Lookup returns the registered type for a template id.
func Lookup(templateID string) (Type, bool) {
	t, ok := registryByKey[templateID]
	return t, ok
}

// DefaultEnabled returns the registry default for a template id. Unregistered ids
// fail OPEN (true) so new templates are never silently dropped before classification.
func DefaultEnabled(templateID string) bool {
	t, ok := registryByKey[templateID]
	if !ok {
		return true
	}
	return t.Class != ClassOptional
}

// IsLocked reports whether the template id is security-critical and never gateable.
func IsLocked(templateID string) bool {
	t, ok := registryByKey[templateID]
	return ok && t.Class == ClassLocked
}
