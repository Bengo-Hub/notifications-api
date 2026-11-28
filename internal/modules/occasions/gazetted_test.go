package occasions

import (
	"context"
	"testing"
	"time"
)

type fakeCalendar map[int]map[string]string

func (f fakeCalendar) Dates(_ context.Context, year int) map[string]string { return f[year] }

// An explicit-date occasion takes a year's missing date from the platform calendar; a date set on
// the occasion wins, and other rule types are left alone.
func TestWithGazetted(t *testing.T) {
	s := (&Service{}).WithGazetted(fakeCalendar{
		2026: {"idd_ul_fitr": "2026-03-21", "labour_day": "2026-05-01"},
		2027: {"idd_ul_fitr": "2027-03-10"},
	})
	fill := s.gazettedFill(context.Background(), time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC))

	idd := withGazetted(View{Key: "idd_ul_fitr", Rule: Rule{Type: RuleExplicit, Dates: map[string]string{"2026": "2026-03-20"}}}, fill)
	if idd.Rule.Dates["2026"] != "2026-03-20" || idd.Rule.Dates["2027"] != "2027-03-10" {
		t.Fatalf("idd dates = %v", idd.Rule.Dates)
	}
	next := withNextDate(idd, time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC), time.UTC)
	if next.NextDate != "2027-03-10" || next.NeedsDate {
		t.Fatalf("next = %s needs=%v", next.NextDate, next.NeedsDate)
	}
	labour := withGazetted(View{Key: "labour_day", Rule: Rule{Type: RuleFixed, Month: 5, Day: 1}}, fill)
	if labour.Rule.Dates != nil {
		t.Fatalf("a fixed rule was changed: %v", labour.Rule)
	}
	if v := withGazetted(View{Key: "idd_ul_azha", Rule: Rule{Type: RuleExplicit}}, nil); v.Rule.Dates != nil {
		t.Fatalf("no calendar: %v", v.Rule.Dates)
	}
}
