package tenanttemplates

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// NamedStatus reports any template name on the tenant's WABA, in or out of the manifest, and
// never writes to Meta.
func TestNamedStatus(t *testing.T) {
	f := &fakeMeta{existing: map[string][2]string{
		"mf_video_ready_btn":    {"APPROVED", "NONE"},
		"mf_daily_brief_btn":    {"REJECTED", "INVALID_FORMAT"},
		"mf_publish_result_btn": {"PENDING", ""},
	}}
	srv := f.server(t)
	s, asked := testService(t, srv.URL, nil, testDefs)

	got, err := s.NamedStatus(context.Background(), "platform", []string{"mf_video_ready_btn", "mf_daily_brief_btn",
		"mf_publish_result_btn", "mf_low_credits_btn", "mf_video_ready_btn", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d statuses, want 4 (duplicates and blanks dropped): %+v", len(got), got)
	}
	want := map[string][2]string{
		"mf_video_ready_btn":    {"APPROVED", ""},
		"mf_daily_brief_btn":    {"REJECTED", "INVALID_FORMAT"},
		"mf_publish_result_btn": {"PENDING", ""},
		"mf_low_credits_btn":    {StatusMissing, ""},
	}
	for _, st := range got {
		if w := want[st.Name]; st.Status != w[0] || st.Reason != w[1] {
			t.Errorf("%s = %s/%s, want %s/%s", st.Name, st.Status, st.Reason, w[0], w[1])
		}
	}
	if len(f.created) != 0 {
		t.Fatal("status must never write to Meta")
	}
	if len(*asked) != 1 || (*asked)[0] != "platform" {
		t.Fatalf("credentials asked for %v", *asked)
	}
	for _, p := range f.paths {
		if !strings.Contains(p, "/WABA-platform/") {
			t.Fatalf("called %s, want the tenant's WABA", p)
		}
	}
}

func TestNamedStatusLimitsAndCredentials(t *testing.T) {
	sel, _ := ParseSelection(defaultSelectionJSON)
	boom := errors.New("no WABA")
	s := New(func(context.Context, string) (string, string, error) { return "", "", boom }, sel, nil)
	if _, err := s.NamedStatus(context.Background(), "t1", nil); err == nil {
		t.Fatal("no names must be refused")
	}
	many := make([]string, MaxStatusNames+1)
	for i := range many {
		many[i] = "n" + strings.Repeat("x", i+1)
	}
	if _, err := s.NamedStatus(context.Background(), "t1", many); err == nil {
		t.Fatal("too many names must be refused")
	}
	if _, err := s.NamedStatus(context.Background(), "t1", []string{"mf_video_ready_btn"}); !errors.Is(err, ErrCredentials) {
		t.Fatalf("err = %v, want ErrCredentials", err)
	}
}
