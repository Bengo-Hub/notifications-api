// Package occasions holds the yearly calendar of greeting occasions (public holidays, Customer
// Service Week, a business's own anniversary) and turns the next one into a broadcast draft.
package occasions

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Rule types. A rule says on which date an occasion starts in a given year.
const (
	RuleFixed         = "fixed"           // {month, day}: 1 Jan, 20 Oct
	RuleNthWeekday    = "nth_weekday"     // {month, weekday, n}: 2nd Sunday of May; n = -1 is the last
	RuleFirstFullWeek = "first_full_week" // {month}: Monday of the first Monday-to-Sunday week inside the month
	RuleEasterOffset  = "easter_offset"   // {days}: days from Western Easter Sunday (Good Friday = -2)
	RuleExplicit      = "explicit"        // {dates: {"2026": "2026-03-20"}}: dates confirmed per year (lunar holidays)
)

// ErrNoDate means the rule has no date for that year (an explicit rule nobody has confirmed yet).
var ErrNoDate = errors.New("no date set for this year")

// Rule is the decoded form of Occasion.rule.
type Rule struct {
	Type    string            `json:"type"`
	Month   int               `json:"month,omitempty"`
	Day     int               `json:"day,omitempty"`
	Weekday int               `json:"weekday,omitempty"` // 0 = Sunday ... 6 = Saturday
	N       int               `json:"n,omitempty"`
	Days    int               `json:"days,omitempty"`
	Dates   map[string]string `json:"dates,omitempty"`
}

// RuleFromMap decodes the JSON column.
func RuleFromMap(m map[string]any) Rule {
	r := Rule{Type: str(m["type"])}
	r.Month = num(m["month"])
	r.Day = num(m["day"])
	r.Weekday = num(m["weekday"])
	r.N = num(m["n"])
	r.Days = num(m["days"])
	if d, ok := m["dates"].(map[string]any); ok {
		r.Dates = map[string]string{}
		for k, v := range d {
			r.Dates[k] = str(v)
		}
	}
	return r
}

// Validate checks a rule before it is saved.
func (r Rule) Validate() error {
	switch r.Type {
	case RuleFixed:
		if r.Month < 1 || r.Month > 12 || r.Day < 1 || r.Day > 31 {
			return fmt.Errorf("fixed rule needs month 1-12 and day 1-31")
		}
		if _, err := r.DateIn(2028, time.UTC); err != nil { // leap year accepts 29 Feb
			return err
		}
	case RuleNthWeekday:
		if r.Month < 1 || r.Month > 12 || r.Weekday < 0 || r.Weekday > 6 || r.N == 0 || r.N < -1 || r.N > 5 {
			return fmt.Errorf("nth_weekday rule needs month 1-12, weekday 0-6 and n 1-5 or -1")
		}
	case RuleFirstFullWeek:
		if r.Month < 1 || r.Month > 12 {
			return fmt.Errorf("first_full_week rule needs month 1-12")
		}
	case RuleEasterOffset:
		if r.Days < -60 || r.Days > 60 {
			return fmt.Errorf("easter_offset days must be within 60 days of Easter")
		}
	case RuleExplicit:
		for y, d := range r.Dates {
			if _, err := strconv.Atoi(y); err != nil {
				return fmt.Errorf("explicit date key %q is not a year", y)
			}
			if _, err := time.Parse("2006-01-02", d); err != nil {
				return fmt.Errorf("explicit date %q is not YYYY-MM-DD", d)
			}
		}
	default:
		return fmt.Errorf("unknown rule type %q", r.Type)
	}
	return nil
}

// DateIn returns the occasion's start date in year, at midnight in loc.
func (r Rule) DateIn(year int, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	switch r.Type {
	case RuleFixed:
		d := time.Date(year, time.Month(r.Month), r.Day, 0, 0, 0, 0, loc)
		if d.Month() != time.Month(r.Month) { // 29 Feb in a common year
			return time.Time{}, ErrNoDate
		}
		return d, nil
	case RuleNthWeekday:
		return nthWeekday(year, time.Month(r.Month), time.Weekday(r.Weekday), r.N, loc)
	case RuleFirstFullWeek:
		first := time.Date(year, time.Month(r.Month), 1, 0, 0, 0, 0, loc)
		shift := (int(time.Monday) - int(first.Weekday()) + 7) % 7
		return first.AddDate(0, 0, shift), nil
	case RuleEasterOffset:
		return Easter(year, loc).AddDate(0, 0, r.Days), nil
	case RuleExplicit:
		s, ok := r.Dates[strconv.Itoa(year)]
		if !ok {
			return time.Time{}, ErrNoDate
		}
		d, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			return time.Time{}, ErrNoDate
		}
		return d, nil
	}
	return time.Time{}, fmt.Errorf("unknown rule type %q", r.Type)
}

// NextOccurrence is the first start date on or after the day of `after` (in loc), looking at
// this year and the next. ErrNoDate when neither year has a date (explicit rule not confirmed).
func (r Rule) NextOccurrence(after time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	a := after.In(loc)
	today := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, loc)
	for _, y := range []int{a.Year(), a.Year() + 1} {
		d, err := r.DateIn(y, loc)
		if errors.Is(err, ErrNoDate) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if !d.Before(today) {
			return d, nil
		}
	}
	return time.Time{}, ErrNoDate
}

func nthWeekday(year int, month time.Month, wd time.Weekday, n int, loc *time.Location) (time.Time, error) {
	if n == -1 {
		last := time.Date(year, month+1, 0, 0, 0, 0, 0, loc)
		back := (int(last.Weekday()) - int(wd) + 7) % 7
		return last.AddDate(0, 0, -back), nil
	}
	first := time.Date(year, month, 1, 0, 0, 0, 0, loc)
	shift := (int(wd) - int(first.Weekday()) + 7) % 7
	d := first.AddDate(0, 0, shift+7*(n-1))
	if d.Month() != month {
		return time.Time{}, ErrNoDate
	}
	return d, nil
}

// Easter is Western (Gregorian) Easter Sunday for year (anonymous Gregorian algorithm).
func Easter(year int, loc *time.Location) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}
