package events

import (
	"encoding/json"
	"strings"
	"testing"
)

// noBlankLabels fails when a report renders a label with nothing after it.
func noBlankLabels(t *testing.T, name, html string) {
	t.Helper()
	for _, bad := range []string{`<td style="padding:6px 0"></td>`, `<td style="padding:4px 0"></td>`, `<td style="padding:4px 12px 4px 0"></td>`,
		"<li></li>", "<strong></strong>", "<p></p>", ", worth </td>", "the oldest  old", "generated .", "for the week of  to"} {
		if strings.Contains(html, bad) {
			t.Errorf("%s renders a blank (%q):\n%s", name, bad, html)
		}
	}
}

func decodeHealth(t *testing.T, raw string) hadiaHealthPayload {
	t.Helper()
	var e hadiaHealthEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	return e.Payload
}

// hadia-api opsreport.dailyFigures and send, as a morning with problems and as a quiet one.
func TestHadiaHealthDailyReport(t *testing.T) {
	p := decodeHealth(t, `{"tenant_id":"6f1c0a2e-0000-4000-8000-000000000001","payload":{"day":"2026-11-12",
		"reconcile":{"registries_differing":2},"failed_payouts_24h":1,"failed_payouts_24h_approximate":true,
		"failed_payouts_24h_basis":"updated_at","cases":{"open":4,"in_progress":1,"overdue":1,"urgent":0},
		"outbox":{"pending":3,"oldest_pending_seconds":900,"failed":0},
		"consumer_lag":{"notif-hadia-ops-health-daily":0,"notifications-hadia":5},
		"responders":["platform_ops"],"generated_at":"2026-11-12T06:00:05Z"}}`)
	if !p.forOps() {
		t.Fatal("a report naming platform_ops must go to ops")
	}
	data, attention := hadiaDailyData(p)
	if len(attention) != 4 {
		t.Fatalf("attention %v, want differing, failed payouts, overdue and the stale outbox", attention)
	}
	subject, text := hadiaDailySummary(p, attention)
	if subject != "Hadia health for 2026-11-12: 4 things need attention" {
		t.Fatalf("subject %q", subject)
	}
	if strings.Contains(text, "\n") || !strings.Contains(text, "2 registries with a gift log that differs from treasury") ||
		!strings.Contains(text, "1 payout failed in the last 24 hours (approximate)") {
		t.Fatalf("text %q", text)
	}
	html := renderEmail(t, hadiaHealthDailyTemplate, data)
	for _, want := range []string{"Hadia health for 2026-11-12", "Needs attention", "1 (approximate)", "the oldest 15 minutes old",
		"(updated_at)", "notifications-hadia", "Report generated 12 Nov 2026, 09:00 EAT"} {
		if !strings.Contains(html, want) {
			t.Errorf("daily lacks %q:\n%s", want, html)
		}
	}
	if strings.Index(html, "notifications-hadia") > strings.Index(html, "notif-hadia-ops-health-daily") {
		t.Error("consumer lag must list the busiest consumer first")
	}
	noBlankLabels(t, "daily", html)

	// A quiet day with no lag read and no time: all clear, and no optional line renders empty.
	quiet := decodeHealth(t, `{"payload":{"day":"2026-11-13","responders":["platform_ops"]}}`)
	data, attention = hadiaDailyData(quiet)
	if len(attention) != 0 {
		t.Fatalf("quiet attention %v", attention)
	}
	if subject, _ := hadiaDailySummary(quiet, attention); subject != "Hadia health for 2026-11-13: all clear" {
		t.Fatalf("quiet subject %q", subject)
	}
	html = renderEmail(t, hadiaHealthDailyTemplate, data)
	if !strings.Contains(html, "All clear") || !strings.Contains(html, "Consumer lag could not be read") {
		t.Errorf("quiet day:\n%s", html)
	}
	for _, absent := range []string{"Needs attention", "approximate", "the oldest", "Report generated"} {
		if strings.Contains(html, absent) {
			t.Errorf("quiet day shows %q:\n%s", absent, html)
		}
	}
	noBlankLabels(t, "quiet daily", html)
}

// hadia-api opsreport.weeklyFigures and send: KES money, open cases by kind, the currency scope.
func TestHadiaHealthWeeklyReport(t *testing.T) {
	p := decodeHealth(t, `{"payload":{"week_from":"2026-11-02","week_to":"2026-11-08",
		"payments":{"count":12,"value":"54000.00"},"payouts":{"count":3,"gross":"50000.00","net":"47500.00"},
		"open_defects":{"other":1,"unmatched_credit":2},"currency":"KES",
		"currency_scope":"KES only: payments and payouts in any other currency are left out",
		"responders":["platform_ops"],"generated_at":"2026-11-09T06:00:00Z"}}`)
	data := hadiaWeeklyData(p)
	if got := hadiaWeeklySubject(p); got != "Hadia weekly health report: 2 Nov 2026 to 8 Nov 2026" {
		t.Fatalf("subject %q", got)
	}
	if got := hadiaWeeklyText(p, data); got != "Hadia week 2 Nov 2026 to 8 Nov 2026: 12 gifts worth KES 54,000; 3 payouts, KES 50,000 gross, KES 47,500 net; 3 cases open." {
		t.Fatalf("text %q", got)
	}
	html := renderEmail(t, hadiaHealthWeeklyTemplate, data)
	for _, want := range []string{"For the week of 2 Nov 2026 to 8 Nov 2026", "12, worth KES 54,000", "KES 50,000", "KES 47,500",
		"unmatched credit", "Money figures: KES only", "Report generated 9 Nov 2026, 09:00 EAT"} {
		if !strings.Contains(html, want) {
			t.Errorf("weekly lacks %q:\n%s", want, html)
		}
	}
	if strings.Index(html, "unmatched credit") > strings.Index(html, "<td style=\"padding:4px 12px 4px 0\">other") {
		t.Error("open cases by kind must list the largest first")
	}
	noBlankLabels(t, "weekly", html)

	// An empty week: zero money, no kinds table, no period line, nothing blank.
	empty := decodeHealth(t, `{"payload":{"responders":["platform_ops"]}}`)
	data = hadiaWeeklyData(empty)
	if hadiaWeeklySubject(empty) != "Hadia weekly health report" {
		t.Fatal("empty subject")
	}
	html = renderEmail(t, hadiaHealthWeeklyTemplate, data)
	if !strings.Contains(html, "0, worth KES 0") {
		t.Errorf("empty week:\n%s", html)
	}
	for _, absent := range []string{"For the week of", "Open cases by kind", "Money figures", "Report generated"} {
		if strings.Contains(html, absent) {
			t.Errorf("empty week shows %q:\n%s", absent, html)
		}
	}
	noBlankLabels(t, "empty weekly", html)
}

func TestHadiaHealthRespondersAndWords(t *testing.T) {
	if (hadiaHealthPayload{Responders: []string{"support"}}).forOps() {
		t.Error("a report for other responders is not for ops")
	}
	if !(hadiaHealthPayload{}).forOps() {
		t.Error("a report naming nobody goes to ops")
	}
	for sec, want := range map[int]string{45: "45 seconds", 60: "1 minute", 900: "15 minutes", 3600: "1 hour", 11100: "3 hours 5 minutes"} {
		if got := readableSeconds(sec); got != want {
			t.Errorf("readableSeconds(%d) = %q, want %q", sec, got, want)
		}
	}
	if hadiaAmount("", "KES") != "KES 0" || hadiaAmount("1250.50", "KES") == "" {
		t.Error("amounts")
	}
}
