package main

import (
	"bytes"
	"html/template"
	"path/filepath"
	"strings"
	"testing"
)

// Each upgrade trial event is mapped, and its template renders every field from the data its
// builder produces (a key the builder drops would print "<no value>").
func TestUpgradeTrialEventsRender(t *testing.T) {
	payload := map[string]any{
		"plan_name": "Duka Pro", "base_plan_name": "Duka Basic", "upgrade_plan_name": "Duka Pro",
		"from_plan_name": "Duka Basic", "days": 14, "days_remaining": 3, "ends_at": "10 Nov 2026",
		"upgrade_at": "30 Nov 2026", "effective_at": "30 Nov 2026", "manage_url": "https://pricing.example/billing",
	}
	for _, ev := range []string{"upgrade_trial_started", "upgrade_trial_ending", "upgrade_trial_ended", "plan_change_scheduled", "trial_ended"} {
		m, ok := subscriptionMappings[ev]
		if !ok || m.DataBuilder == nil {
			t.Fatalf("%s is not mapped", ev)
		}
		data := m.DataBuilder(payload, "https://tenant.example")
		if ev != "trial_ended" && data["action_link"] != "https://pricing.example/billing" {
			t.Fatalf("%s action link %v", ev, data["action_link"])
		}
		file := filepath.Join("..", "..", "templates", "email", filepath.FromSlash(m.TemplateID)+".html")
		tpl, err := template.ParseFiles(file)
		if err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
		var out bytes.Buffer
		if err := tpl.ExecuteTemplate(&out, "content", data); err != nil {
			t.Fatalf("%s render: %v", ev, err)
		}
		if ev != "trial_ended" && strings.Contains(out.String(), "<no value>") {
			t.Fatalf("%s renders a missing field:\n%s", ev, out.String())
		}
	}

	// Plan change emails name the plans, not their ids.
	data := subscriptionMappings["upgraded"].DataBuilder(map[string]any{
		"new_plan_code": "POWERSUITE_DUKA_PRO", "new_plan_name": "Duka Pro",
		"old_plan_id": "8a1c", "old_plan_name": "Duka Basic",
	}, "https://tenant.example")
	if data["new_plan_name"] != "Duka Pro" || data["old_plan_name"] != "Duka Basic" {
		t.Fatalf("upgraded names %+v", data)
	}

	a := subscriptionEvent{ID: "e1", EventType: "upgrade_trial_started", AggregateID: "s1"}
	b := subscriptionEvent{ID: "e2", EventType: "upgrade_trial_started", AggregateID: "s1"}
	if subscriptionIdempotencyKey(a) == subscriptionIdempotencyKey(b) {
		t.Fatal("two trials on one subscription must not share a key")
	}
}
