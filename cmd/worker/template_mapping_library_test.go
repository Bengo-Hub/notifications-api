package main

import (
	"strings"
	"testing"
)

// Library emails from the 12 November 2026 template audit (see template_mapping_audit_test.go).
func TestLibraryEmailsHaveNoBlanks(t *testing.T) {
	f := libraryMappings["library.fine.assessed"]
	d := f.DataBuilder(map[string]interface{}{"member_id": "m-1", "amount": "50.00", "reason": "OVERDUE"}, "https://t.example")
	out := renderEmail(t, f.TemplateID, d)
	mustContain(t, "fine", out, "Overdue loan", "50.00", "a library account")
	if strings.Contains(out, "m-1") {
		t.Fatalf("fine email shows the raw member id:\n%s", out)
	}
	d["member_name"] = "Jane Wanjiru"
	mustContain(t, "fine with member", renderEmail(t, f.TemplateID, d), "Jane Wanjiru")

	fee := libraryMappings["library.membership.fee_due"]
	mustContain(t, "fee_due", renderEmail(t, fee.TemplateID, fee.DataBuilder(map[string]interface{}{"amount": "1000", "expires_at": "2026-12-01T00:00:00Z"}, "https://t.example")), "01 Dec 2026")
	if out := renderEmail(t, fee.TemplateID, fee.DataBuilder(map[string]interface{}{"amount": "1000"}, "https://t.example")); strings.Contains(out, "Membership Expires") {
		t.Fatalf("fee_due shows an empty expiry line:\n%s", out)
	}
	s := libraryMappings["library.serial.issue_late"]
	mustContain(t, "serial", renderEmail(t, s.TemplateID, s.DataBuilder(map[string]interface{}{"issue_no": "Vol 3 No 2", "expected_date": "2026-11-01"}, "https://t.example")), "Vol 3 No 2")
}
