package main

import (
	"bytes"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// A learner under 18 with a phone of their own and a parent with a phone and an email.
func minorPayload(extra map[string]any) map[string]any {
	p := map[string]any{
		"studentId": "DGT-ABCD2345", "studentName": "Clara Jessica Otieno", "studentEmail": "dgt-abcd2345@no-email.invalid",
		"studentPhone": "+254720336101", "guardianName": "Sarah Digo", "guardianEmail": "sarah@example.com", "guardianPhone": "+254708244385",
		"courseName": "Tech Explorers", "currency": "KES", "totalAmount": 8000.0, "portalLink": "https://codevertexafrica.com/digitika/success?reference=DGT-4-DGT-ABCD2345",
		"installmentNo": 2.0, "totalInstallments": 2.0, "amount": 4000.0, "amountPaid": 4000.0, "remainingBalance": 4000.0,
		"dueDate": "2026-11-15", "daysUntilDue": 3.0, "paymentPlan": "2-installments",
		"certificateNumber": "DGT-2026-000123", "certificateLink": "https://codevertexafrica.com/certificates/9f4c2a7e",
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// A website contact form enquiry, as codevertex-website publishes it.
func enquiryPayload(extra map[string]any) map[string]any {
	p := map[string]any{
		"name": "Jane Achieng", "email": "jane@example.com", "phone": "254712345678",
		"service": "pos", "serviceLabel": "POS and inventory", "message": "We run two shops in Kisumu.", "source": "contact",
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestEnquiryRepliesToTheVisitorAndAlertsTheTeamByEmail(t *testing.T) {
	n, ok := buildContactReceived(enquiryPayload(nil))
	if !ok || len(n.Parties) != 1 {
		t.Fatalf("one visitor party: ok=%v parties=%d", ok, len(n.Parties))
	}
	wa := n.Parties[0].WhatsApp
	if wa == nil || wa.Name != "codevertex_enquiry_received_v1_btn" || wa.Button != "services" {
		t.Fatalf("visitor WhatsApp = %+v", wa)
	}
	if wa.Params[1] != "POS and inventory" {
		t.Errorf("the reply names the readable service, got %q", wa.Params[1])
	}

	old, _ := buildContactReceived(enquiryPayload(map[string]any{"serviceLabel": "", "service": ""}))
	if got := old.Parties[0].WhatsApp.Params[1]; got != "our services" {
		t.Errorf("no service falls back to a readable phrase, got %q", got)
	}

	cfg := &config.Config{}
	cfg.Events.PlatformAlertEmails = " team@example.com , ,bad-address, ops@example.com.invalid,owner@example.com"
	alerts := platformAlertParties(cfg, n)
	if len(alerts) != 2 {
		t.Fatalf("two usable alert addresses, got %d", len(alerts))
	}
	for _, a := range alerts {
		if len(a.Channels) != 1 || a.Channels[0] != "email" || a.WhatsApp != nil || a.TemplateID != "platform/website_enquiry" {
			t.Errorf("the team alert is email only on its own template: %+v", a)
		}
	}
	if alerts[0].Key == alerts[1].Key {
		t.Error("each alert address has its own idempotency key")
	}
	cfg.Events.PlatformAlertEmails = ""
	if len(platformAlertParties(cfg, n)) != 0 {
		t.Error("an empty PLATFORM_ALERT_EMAILS switches the alert off")
	}
	if other, _ := buildCertificateIssued(minorPayload(nil)); len(platformAlertParties(&config.Config{}, other)) != 0 {
		t.Error("notices without an alert template never email the team")
	}

	tpl, err := template.ParseFiles("../../templates/email/platform/website_enquiry.html")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.ExecuteTemplate(&out, "content", n.Data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Jane Achieng", "+254712345678", "POS and inventory", "two shops in Kisumu", "/admin/contacts"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("team alert lacks %q", want)
		}
	}
}

func partyByKey(n digitikaNotice, key string) *noticeParty {
	for i := range n.Parties {
		if n.Parties[i].Key == key {
			return &n.Parties[i]
		}
	}
	return nil
}

func TestMinorReminderReachesTheChildAndTheParentOnTheirOwnContacts(t *testing.T) {
	n, ok := buildInstallmentReminder(minorPayload(nil))
	if !ok || len(n.Parties) != 2 {
		t.Fatalf("a minor with both contacts has two parties: ok=%v parties=%d", ok, len(n.Parties))
	}
	child, parent := partyByKey(n, "student"), partyByKey(n, "guardian")
	if child == nil || parent == nil {
		t.Fatal("student and guardian parties expected")
	}
	// The child has no real email (placeholder), so only WhatsApp; the parent has both.
	cc, pc := partyContacts(*child), partyContacts(*parent)
	if _, has := cc["email"]; has || cc["whatsapp"] != "+254720336101" {
		t.Fatalf("child contacts = %v", cc)
	}
	if pc["email"] != "sarah@example.com" || pc["whatsapp"] != "+254708244385" {
		t.Fatalf("parent contacts = %v", pc)
	}
	// Each is greeted by name and worded for themselves.
	if child.Data["name"] != "Clara Jessica Otieno" || child.Data["for_student"] != "" {
		t.Fatalf("child data = %v", child.Data)
	}
	if parent.Data["name"] != "Sarah Digo" || parent.Data["for_student"] != "Clara Jessica Otieno" {
		t.Fatalf("parent data = %v", parent.Data)
	}
	if got := child.WhatsApp.Params[3]; got != "your" {
		t.Fatalf("the child reads your place, got %q", got)
	}
	if got := parent.WhatsApp.Params[3]; got != "Clara's" {
		t.Fatalf("the parent reads Clara's place, got %q", got)
	}
	if child.WhatsApp.Button != "digitika/success?reference=DGT-4-DGT-ABCD2345" {
		t.Fatalf("button suffix = %q", child.WhatsApp.Button)
	}
}

func TestReminderWordingFollowsDueDate(t *testing.T) {
	soon, _ := buildInstallmentReminder(minorPayload(nil))
	if soon.WATemplateID != "" || soon.Parties[0].WhatsApp.Name != "digitika_installment_reminder_v1_btn" || soon.Data["overdue"] != false {
		t.Fatalf("due soon: %+v", soon)
	}
	late, _ := buildInstallmentReminder(minorPayload(map[string]any{"daysUntilDue": -4.0}))
	if late.WATemplateID != "digitika/installment_overdue" || late.Parties[0].WhatsApp.Name != "digitika_installment_overdue_v1_btn" ||
		late.Data["overdue"] != true || late.Data["days_overdue"] != 4 {
		t.Fatalf("overdue: %+v", late)
	}
	if soon.Parties[0].WhatsApp.Params[5] != "15 Nov 2026" {
		t.Fatalf("the date reads for people, got %q", soon.Parties[0].WhatsApp.Params[5])
	}
}

func TestAnAdultWithNoGuardianGetsOneParty(t *testing.T) {
	n, ok := buildInstallmentReminder(minorPayload(map[string]any{
		"studentEmail": "adult@example.com", "guardianName": "", "guardianEmail": "", "guardianPhone": "",
	}))
	if !ok || len(n.Parties) != 1 || n.Parties[0].Key != "student" {
		t.Fatalf("adult parties = %+v", n.Parties)
	}
}

func TestOneSharedPhoneGetsOneMessage(t *testing.T) {
	n, _ := buildInstallmentReminder(minorPayload(map[string]any{"guardianPhone": "+254720336101", "guardianEmail": ""}))
	if len(n.Parties) != 1 {
		t.Fatalf("a guardian with only the child's own phone adds nothing: %+v", n.Parties)
	}
}

func TestNobodyToNotifyIsNotSent(t *testing.T) {
	_, ok := buildEnrollmentConfirmed(map[string]any{"studentName": "X", "studentEmail": "", "courseName": "C"})
	if ok {
		t.Fatal("no contact at all must not be reported as reachable")
	}
}

func TestReceiptHasAPaidInFullVariant(t *testing.T) {
	part, _ := buildInstallmentReceipt(minorPayload(nil))
	if part.WATemplateID != "" || part.Parties[0].WhatsApp.Name != "digitika_installment_receipt_v1_btn" || part.Data["remaining_balance"] != "4000" {
		t.Fatalf("part payment: %+v", part)
	}
	full, _ := buildInstallmentReceipt(minorPayload(map[string]any{"remainingBalance": 0.0}))
	if full.WATemplateID != "digitika/payment_complete" || full.Parties[0].WhatsApp.Name != "digitika_payment_complete_v1_btn" || full.Data["remaining_balance"] != "" {
		t.Fatalf("paid in full: %+v", full)
	}
}

func TestPortalAccountKeepsThePasswordOffWhatsApp(t *testing.T) {
	n, ok := buildPortalAccount(map[string]any{
		"accountRole": "parent", "name": "Sarah Digo", "email": "sarah@example.com", "phone": "+254708244385",
		"studentName": "Clara", "tempPassword": "Tmp-9x2Q", "loginUrl": "https://codevertexafrica.com/digitika/parent", "courseName": "Tech Explorers",
	})
	if !ok || len(n.Parties) != 1 {
		t.Fatalf("one account owner: %+v", n)
	}
	wa := n.Parties[0].WhatsApp
	for _, param := range wa.Params {
		if strings.Contains(param, "Tmp-9x2Q") {
			t.Fatal("a password must never be a WhatsApp parameter")
		}
	}
	if wa.Button != "digitika/parent" || n.Data["temp_password"] != "Tmp-9x2Q" {
		t.Fatalf("button %q, data %v", wa.Button, n.Data)
	}
}

func TestAccessCodeGoesFromThePlatformNumber(t *testing.T) {
	n, ok := buildAccessCode(map[string]any{"name": "Sarah", "phone": "+254708244385", "code": "482913", "ttlMinutes": 10.0})
	if !ok || n.Scope == "" || !n.Ungated || n.Parties[0].WhatsApp.Name != "auth_otp" || n.Parties[0].WhatsApp.Button != "482913" {
		t.Fatalf("access code = %+v", n)
	}
	if _, ok := buildAccessCode(map[string]any{"name": "Sarah", "code": "482913"}); ok {
		t.Fatal("a code with nowhere to go is not reachable")
	}
}

func TestButtonsStayOnTheDigitikaSite(t *testing.T) {
	if got := digitikaButton("https://codevertexafrica.com/student"); got != "student" {
		t.Fatalf("suffix = %q", got)
	}
	if got := digitikaButton("https://evil.example.com/student"); got != "" {
		t.Fatalf("a foreign link must not become a button, got %q", got)
	}
	if got := digitikaButton("https://codevertexafrica.com"); got != "" {
		t.Fatalf("the bare site has no suffix, got %q", got)
	}
}

// Every WhatsApp template a Digitika notice names exists in the manifest with exactly as many
// parameters as its body declares, and every button template carries its suffix, as Meta
// rejects anything else and the customer simply never gets the message.
func TestDigitikaWhatsAppTemplatesMatchTheManifest(t *testing.T) {
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	placeholder := regexp.MustCompile(`\{\{(\d+)\}\}`)
	declared := map[string]int{}
	for _, d := range defs {
		seen := map[string]bool{}
		for _, m := range placeholder.FindAllStringSubmatch(d.Body, -1) {
			seen[m[1]] = true
		}
		declared[d.Name] = len(seen)
	}
	payloads := map[string]map[string]any{
		"enrollment.confirmed": minorPayload(nil),
		"payment.succeeded":    minorPayload(nil),
		"payment.complete":     minorPayload(map[string]any{"remainingBalance": 0.0}),
		"installment.reminder": minorPayload(nil),
		"installment.overdue":  minorPayload(map[string]any{"daysUntilDue": -2.0}),
		"certificate.issued":   minorPayload(nil),
		"contact.received":     enquiryPayload(nil),
	}
	builders := map[string]func(map[string]any) (digitikaNotice, bool){
		"enrollment.confirmed": buildEnrollmentConfirmed, "payment.succeeded": buildInstallmentReceipt, "payment.complete": buildInstallmentReceipt,
		"installment.reminder": buildInstallmentReminder, "installment.overdue": buildInstallmentReminder, "certificate.issued": buildCertificateIssued,
		"contact.received": buildContactReceived,
	}
	for name, p := range payloads {
		n, _ := builders[name](p)
		for _, party := range n.Parties {
			wa := party.WhatsApp
			want, ok := declared[wa.Name]
			if !ok {
				t.Errorf("%s: template %q is not in templates.json", name, wa.Name)
				continue
			}
			if len(wa.Params) != want {
				t.Errorf("%s/%s: %q expects %d parameters, sends %d", name, party.Key, wa.Name, want, len(wa.Params))
			}
			for i, v := range wa.Params {
				if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\n\r") {
					t.Errorf("%s/%s: parameter %d of %q is empty or has a newline: %q", name, party.Key, i+1, wa.Name, v)
				}
			}
			if strings.HasSuffix(wa.Name, "_btn") && wa.Button == "" {
				t.Errorf("%s/%s: %q has no button suffix", name, party.Key, wa.Name)
			}
		}
		// The text file the free-form fallback renders exists for this notice.
		file := n.WATemplateID
		if file == "" {
			file = n.TemplateID
		}
		if _, err := os.Stat("../../templates/whatsapp/" + file + ".txt"); err != nil {
			t.Errorf("%s: missing WhatsApp text file for %s", name, file)
		}
		if _, err := os.Stat("../../templates/email/" + n.TemplateID + ".html"); err != nil {
			t.Errorf("%s: missing email template for %s", name, n.TemplateID)
		}
	}
	for _, name := range []string{"digitika_portal_account_v2_btn", "codevertex_enquiry_received_v1_btn"} {
		if _, ok := declared[name]; !ok {
			t.Errorf("%q is not in templates.json", name)
		}
	}
}

func TestDigitikaEmailsRenderForTheLearnerAndTheParent(t *testing.T) {
	render := func(id string, data map[string]any) string {
		tpl, err := template.ParseFiles("../../templates/email/" + id + ".html")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := tpl.ExecuteTemplate(&out, "content", data); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	n, _ := buildInstallmentReminder(minorPayload(map[string]any{"daysUntilDue": -3.0}))
	data := func(p noticeParty) map[string]any {
		d := map[string]any{}
		for k, v := range n.Data {
			d[k] = v
		}
		for k, v := range p.Data {
			d[k] = v
		}
		return d
	}
	parent := render(n.TemplateID, data(*partyByKey(n, "guardian")))
	for _, want := range []string{"Hello Sarah Digo", "Clara Jessica Otieno's place", "overdue", "Overdue by 3 days", "15 Nov 2026", "KES 4000"} {
		if !strings.Contains(parent, want) {
			t.Errorf("parent reminder lacks %q", want)
		}
	}
	child := render(n.TemplateID, data(*partyByKey(n, "student")))
	if !strings.Contains(child, "your place in") || strings.Contains(child, "'s place") {
		t.Error("the learner's reminder is worded for them")
	}
	if strings.Contains(child, "Due in -") || strings.Contains(child, "-3 day") {
		t.Error("an overdue reminder must never say due in a negative number of days")
	}

	soon, _ := buildInstallmentReminder(minorPayload(map[string]any{"daysUntilDue": 0.0}))
	if out := render(soon.TemplateID, soon.Data); !strings.Contains(out, "Due today") {
		t.Error("a payment due today says so")
	}

	paid, _ := buildInstallmentReceipt(minorPayload(map[string]any{"remainingBalance": 0.0}))
	if out := render(paid.TemplateID, paid.Data); !strings.Contains(out, "Paid in full") {
		t.Error("a receipt that clears the balance says paid in full")
	}
	cert, _ := buildCertificateIssued(minorPayload(nil))
	if out := render(cert.TemplateID, cert.Data); !strings.Contains(out, "DGT-2026-000123") {
		t.Error("the certificate email shows its number")
	}
}
