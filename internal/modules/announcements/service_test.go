package announcements

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/ent"
)

func ann(title string, prio int, services []string, starts time.Time, ends *time.Time, active bool) *ent.Announcement {
	return &ent.Announcement{ID: uuid.New(), Title: title, Priority: prio, Services: services, StartsAt: starts, EndsAt: ends, IsActive: active}
}

func titles(list []*ent.Announcement) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Title)
	}
	return out
}

func TestActiveFor(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	ended := now.Add(-time.Minute)
	list := []*ent.Announcement{
		ann("payhero", 10, []string{"pos", "treasury"}, past, &future, true),
		ann("everyone", 0, nil, past.Add(-time.Hour), nil, true),
		ann("newer-everyone", 0, nil, past, nil, true),
		ann("inventory-only", 50, []string{"inventory"}, past, nil, true),
		ann("not-started", 99, nil, future, nil, true),
		ann("ended", 99, nil, past, &ended, true),
		ann("switched-off", 99, nil, past, nil, false),
	}

	got := titles(ActiveFor(list, "POS", now))
	want := []string{"payhero", "newer-everyone", "everyone"}
	if len(got) != len(want) {
		t.Fatalf("pos sees %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pos sees %v, want %v (priority, then newest)", got, want)
		}
	}
	if got := titles(ActiveFor(list, "inventory", now)); len(got) != 3 || got[0] != "inventory-only" {
		t.Fatalf("inventory sees %v", got)
	}
	if got := titles(ActiveFor(list, "logistics", now)); len(got) != 2 {
		t.Fatalf("an app no banner targets sees only the every-app ones, got %v", got)
	}
}

func TestNormalize(t *testing.T) {
	base := func() Input {
		return Input{Title: " PayHero ", Summary: " New gateway ", Services: []string{"POS", "pos", " treasury "}, Highlights: []string{" a ", "", "b"}}
	}
	in := base()
	if err := in.Normalize(); err != nil {
		t.Fatal(err)
	}
	if in.Title != "PayHero" || in.Audience != "all" || in.Tone != "feature" {
		t.Fatalf("defaults/trim wrong: %+v", in)
	}
	if len(in.Services) != 2 || in.Services[0] != "pos" || in.Services[1] != "treasury" {
		t.Fatalf("services = %v", in.Services)
	}
	if len(in.Highlights) != 2 {
		t.Fatalf("highlights = %v", in.Highlights)
	}

	starts := time.Now()
	bad := map[string]func(*Input){
		"no title":          func(i *Input) { i.Title = " " },
		"no summary":        func(i *Input) { i.Summary = "" },
		"label without url": func(i *Input) { i.CTALabel = "Set up" },
		"javascript link":   func(i *Input) { i.CTALabel, i.CTAURL = "x", "javascript:alert(1)" },
		"http link":         func(i *Input) { i.CTALabel, i.CTAURL = "x", "http://example.com" },
		"protocol-relative": func(i *Input) { i.CTALabel, i.CTAURL = "x", "//evil.example" },
		"bad audience":      func(i *Input) { i.Audience = "cashiers" },
		"bad tone":          func(i *Input) { i.Tone = "loud" },
		"ends before start": func(i *Input) { e := starts.Add(-time.Hour); i.StartsAt, i.EndsAt = &starts, &e },
	}
	for name, mutate := range bad {
		in := base()
		mutate(&in)
		if err := in.Normalize(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	for _, link := range []string{"/settings", "https://books.codevertexafrica.com/{orgSlug}/settings?tab=payments"} {
		in := base()
		in.CTALabel, in.CTAURL = "Set up", link
		if err := in.Normalize(); err != nil {
			t.Errorf("%s rejected: %v", link, err)
		}
	}
}
