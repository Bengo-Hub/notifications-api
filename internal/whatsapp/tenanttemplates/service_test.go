package tenanttemplates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// fakeMeta answers like Meta's template endpoints for one WABA and remembers what was created.
type fakeMeta struct {
	mu       sync.Mutex
	existing map[string][2]string // name -> status, rejected_reason
	created  []string
	auth     []string
	paths    []string
}

func (f *fakeMeta) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			data := []map[string]string{}
			for name, st := range f.existing {
				data = append(data, map[string]string{"name": name, "status": st[0], "rejected_reason": st[1]})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body.Name)
		if f.existing == nil {
			f.existing = map[string][2]string{}
		}
		f.existing[body.Name] = [2]string{"PENDING", ""}
		_, _ = w.Write([]byte(`{"id":"1","status":"PENDING"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testService(t *testing.T, graph string, services []string, defs []templatesync.TemplateDef) (*Service, *[]string) {
	t.Helper()
	sel, err := ParseSelection(defaultSelectionJSON)
	if err != nil {
		t.Fatal(err)
	}
	asked := &[]string{}
	creds := func(_ context.Context, tenantID string) (string, string, error) {
		*asked = append(*asked, tenantID)
		return "WABA-" + tenantID, "token-" + tenantID, nil
	}
	usage := func(context.Context, string) ([]string, error) { return services, nil }
	s := New(creds, sel, nil, usage)
	s.GraphURL = graph
	s.manifest = func() ([]templatesync.TemplateDef, error) { return defs, nil }
	return s, asked
}

func TestTenantTemplateStatus(t *testing.T) {
	f := &fakeMeta{existing: map[string][2]string{
		"auth_otp":                          {"APPROVED", "NONE"},
		"ordering_order_placed_v3_btn":      {"REJECTED", "INVALID_FORMAT"},
		"subscription_invoice_ready_v1_btn": {"PENDING", ""},
		"pos_receipt_share_btn":             {"APPROVED", ""}, // on the WABA but not selected
	}}
	srv := f.server(t)
	s, _ := testService(t, srv.URL, []string{"ordering"}, testDefs)

	rep, err := s.TenantTemplateStatus(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.WABAID != "WABA-t1" {
		t.Fatalf("waba %q", rep.WABAID)
	}
	got := map[string]TemplateStatus{}
	for _, ts := range rep.Templates {
		got[ts.Name] = ts
	}
	if got["auth_otp"].Status != "APPROVED" || got["auth_otp"].Reason != "" {
		t.Errorf("auth_otp = %+v", got["auth_otp"])
	}
	if r := got["ordering_order_placed_v3_btn"]; r.Status != "REJECTED" || r.Reason != "INVALID_FORMAT" {
		t.Errorf("ordering = %+v", r)
	}
	if got["broadcast_update_v1"].Status != StatusMissing {
		t.Errorf("broadcast = %+v", got["broadcast_update_v1"])
	}
	if _, ok := got["pos_receipt_share_btn"]; ok {
		t.Error("a template the tenant does not use must not be listed")
	}
	if rep.Summary["APPROVED"] != 1 || rep.Summary["REJECTED"] != 1 || rep.Summary["PENDING"] != 1 || rep.Summary[StatusMissing] != 3 {
		t.Errorf("summary %v", rep.Summary)
	}
	if len(f.created) != 0 {
		t.Fatal("status must never write to Meta")
	}
	for _, a := range f.auth {
		if a != "Bearer token-t1" {
			t.Fatalf("used %q, want the tenant's own token", a)
		}
	}
	for _, p := range f.paths {
		if !strings.Contains(p, "/WABA-t1/") {
			t.Fatalf("called %s, want the tenant's WABA", p)
		}
	}
}

func TestSyncTenantTemplatesDryRunAndLive(t *testing.T) {
	f := &fakeMeta{existing: map[string][2]string{"auth_otp": {"APPROVED", ""}}}
	srv := f.server(t)
	s, _ := testService(t, srv.URL, []string{"ordering"}, testDefs)

	dry, err := s.SyncTenantTemplates(context.Background(), "t1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 0 {
		t.Fatalf("dry run created %v", f.created)
	}
	if dry.Summary["would_create"] != 5 || dry.Summary["skipped"] != 1 {
		t.Fatalf("dry summary %v", dry.Summary)
	}

	live, err := s.SyncTenantTemplates(context.Background(), "t1", false)
	if err != nil {
		t.Fatal(err)
	}
	if live.Summary["created"] != 5 || live.Remaining != 0 {
		t.Fatalf("live summary %v remaining %d", live.Summary, live.Remaining)
	}
	for _, n := range f.created {
		if strings.HasPrefix(n, "pos_") || strings.HasPrefix(n, "codevertex_") || strings.HasPrefix(n, "hadia_") {
			t.Fatalf("created %s, which the tenant does not use", n)
		}
	}
	if len(live.Services) != 1 || live.Services[0] != "ordering" {
		t.Fatalf("services %v", live.Services)
	}
}

// One call submits at most BatchSize; AutoSync keeps going until nothing is left.
func TestAutoSyncSubmitsEveryBatch(t *testing.T) {
	defs := []templatesync.TemplateDef{}
	for i := 0; i < BatchSize+3; i++ {
		defs = append(defs, templatesync.TemplateDef{Name: fmt.Sprintf("ordering_t%02d", i), Category: "UTILITY"})
	}
	f := &fakeMeta{}
	srv := f.server(t)
	s, _ := testService(t, srv.URL, []string{"ordering"}, defs)

	first, err := s.SyncTenantTemplates(context.Background(), "t1", true)
	if err != nil || first.Summary["would_create"] != BatchSize+3 {
		t.Fatalf("dry %v %v", first, err)
	}
	s.AutoSync(context.Background(), "t1")
	if len(f.created) != BatchSize+3 {
		t.Fatalf("created %d, want %d", len(f.created), BatchSize+3)
	}
}

func TestSyncTenantTemplatesCredentialError(t *testing.T) {
	sel, _ := ParseSelection(defaultSelectionJSON)
	boom := errors.New("no WABA")
	s := New(func(context.Context, string) (string, string, error) { return "", "", boom }, sel, nil)
	_, err := s.SyncTenantTemplates(context.Background(), "t1", true)
	if !errors.Is(err, ErrCredentials) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want ErrCredentials wrapping the cause", err)
	}
	if _, err := s.TenantTemplateStatus(context.Background(), "t1"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("status err = %v", err)
	}
}

// A usage source that fails is skipped: the tenant still gets the shared set and the other sources.
func TestUsageSourceErrorIsSkipped(t *testing.T) {
	f := &fakeMeta{}
	srv := f.server(t)
	sel, _ := ParseSelection(defaultSelectionJSON)
	creds := func(context.Context, string) (string, string, error) { return "W", "T", nil }
	bad := func(context.Context, string) ([]string, error) { return nil, errors.New("db down") }
	good := func(context.Context, string) ([]string, error) { return []string{"pos"}, nil }
	s := New(creds, sel, nil, bad, good)
	s.GraphURL = srv.URL
	s.manifest = func() ([]templatesync.TemplateDef, error) { return testDefs, nil }
	rep, err := s.TenantTemplateStatus(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Templates) != len(shared)+1 || rep.Services[0] != "pos" {
		t.Fatalf("templates %d services %v", len(rep.Templates), rep.Services)
	}
}
