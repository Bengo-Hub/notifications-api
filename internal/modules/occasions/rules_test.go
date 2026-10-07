package occasions

import (
	"errors"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

func TestDateIn(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		year int
		want string
	}{
		{"customer service week 2026", Rule{Type: RuleFirstFullWeek, Month: 10}, 2026, "2026-10-05"},
		{"customer service week 2027", Rule{Type: RuleFirstFullWeek, Month: 10}, 2027, "2027-10-04"},
		{"first full week when the 1st is a Monday", Rule{Type: RuleFirstFullWeek, Month: 6}, 2026, "2026-06-01"},
		{"mashujaa", Rule{Type: RuleFixed, Month: 10, Day: 20}, 2026, "2026-10-20"},
		{"good friday 2026", Rule{Type: RuleEasterOffset, Days: -2}, 2026, "2026-04-03"},
		{"easter monday 2026", Rule{Type: RuleEasterOffset, Days: 1}, 2026, "2026-04-06"},
		{"easter 2027", Rule{Type: RuleEasterOffset}, 2027, "2027-03-28"},
		{"mothers day (2nd sunday may) 2026", Rule{Type: RuleNthWeekday, Month: 5, Weekday: 0, N: 2}, 2026, "2026-05-10"},
		{"fathers day (3rd sunday june) 2026", Rule{Type: RuleNthWeekday, Month: 6, Weekday: 0, N: 3}, 2026, "2026-06-21"},
		{"last friday of november 2026", Rule{Type: RuleNthWeekday, Month: 11, Weekday: 5, N: -1}, 2026, "2026-11-27"},
		{"idd explicit", Rule{Type: RuleExplicit, Dates: map[string]string{"2026": "2026-03-20"}}, 2026, "2026-03-20"},
	}
	for _, c := range cases {
		got, err := c.rule.DateIn(c.year, time.UTC)
		if err != nil || got.Format("2006-01-02") != c.want {
			t.Errorf("%s: got %s, %v; want %s", c.name, got.Format("2006-01-02"), err, c.want)
		}
	}
}

func TestNextOccurrence(t *testing.T) {
	csw := Rule{Type: RuleFirstFullWeek, Month: 10}
	if d, _ := csw.NextOccurrence(day("2026-10-05"), time.UTC); d.Format("2006-01-02") != "2026-10-05" {
		t.Errorf("same day counts as next: %s", d)
	}
	if d, _ := csw.NextOccurrence(day("2026-10-07"), time.UTC); d.Format("2006-01-02") != "2027-10-04" {
		t.Errorf("after the start rolls to next year: %s", d)
	}
	idd := Rule{Type: RuleExplicit, Dates: map[string]string{"2026": "2026-03-20"}}
	if _, err := idd.NextOccurrence(day("2026-10-07"), time.UTC); !errors.Is(err, ErrNoDate) {
		t.Errorf("explicit rule without next year's date must report ErrNoDate, got %v", err)
	}
	leap := Rule{Type: RuleFixed, Month: 2, Day: 29}
	if d, err := leap.NextOccurrence(day("2026-03-01"), time.UTC); err != nil || d.Year() != 2027 && d.Year() != 2028 {
		// 2027 has no 29 Feb, so the next is beyond the two-year window: ErrNoDate is acceptable.
		if !errors.Is(err, ErrNoDate) {
			t.Errorf("29 Feb: %s %v", d, err)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := []Rule{
		{Type: RuleFixed, Month: 12, Day: 25},
		{Type: RuleFixed, Month: 2, Day: 29},
		{Type: RuleNthWeekday, Month: 5, Weekday: 0, N: 2},
		{Type: RuleFirstFullWeek, Month: 10},
		{Type: RuleEasterOffset, Days: -2},
		{Type: RuleExplicit, Dates: map[string]string{"2027": "2027-03-10"}},
	}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v should be valid: %v", r, err)
		}
	}
	bad := []Rule{
		{Type: RuleFixed, Month: 13, Day: 1},
		{Type: RuleFixed, Month: 2, Day: 30},
		{Type: RuleNthWeekday, Month: 5, Weekday: 7, N: 2},
		{Type: RuleNthWeekday, Month: 5, Weekday: 0, N: 0},
		{Type: RuleExplicit, Dates: map[string]string{"next": "2027-03-10"}},
		{Type: "lunar"},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v should be invalid", r)
		}
	}
}

func TestRuleFromMapDecodesJSONNumbers(t *testing.T) {
	r := RuleFromMap(map[string]any{"type": "nth_weekday", "month": float64(5), "weekday": float64(0), "n": float64(2)})
	if r.Month != 5 || r.N != 2 || r.Type != RuleNthWeekday {
		t.Fatalf("decoded %+v", r)
	}
}
