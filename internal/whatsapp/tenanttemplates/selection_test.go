package tenanttemplates

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

var testDefs = []templatesync.TemplateDef{
	{Name: "auth_otp", Category: "AUTHENTICATION"},
	{Name: "subscription_invoice_ready_v1_btn", Category: "UTILITY"},
	{Name: "credits_balance_low_v1_btn", Category: "UTILITY"},
	{Name: "broadcast_update_v1", Category: "MARKETING"},
	{Name: "occasion_new_year_v2", Category: "MARKETING"},
	{Name: "ordering_order_placed_v3_btn", Category: "UTILITY"},
	{Name: "pos_receipt_share_btn", Category: "UTILITY"},
	{Name: "finance_invoice_sent", Category: "UTILITY"},
	{Name: "hrm_leave_approved", Category: "UTILITY"},
	{Name: "payroll_payslip_ready", Category: "UTILITY"},
	{Name: "hadia_registry_published_v1_btn", Category: "UTILITY"},
	{Name: "maskani_bill_issued_v1_btn", Category: "UTILITY"},
	{Name: "codevertex_enquiry_received_v1_btn", Category: "UTILITY"},
	{Name: "provider_balance_low_v1_btn", Category: "UTILITY"},
}

var shared = []string{"auth_otp", "broadcast_update_v1", "credits_balance_low_v1_btn", "occasion_new_year_v2", "subscription_invoice_ready_v1_btn"}

func names(defs []templatesync.TemplateDef) []string {
	out := []string{}
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func with(extra ...string) []string {
	out := append(append([]string{}, shared...), extra...)
	return sortStrings(out)
}

func sortStrings(in []string) []string {
	out := append([]string{}, in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Owner decision "Only what the tenant uses": the shared set always, plus each used service's
// prefixes; platform-only templates never.
func TestSelectByService(t *testing.T) {
	sel, err := ParseSelection(defaultSelectionJSON)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		services []string
		want     []string
		matched  []string
	}{
		{name: "no services gets the shared set", services: nil, want: with(), matched: nil},
		{name: "ordering", services: []string{"ordering"}, want: with("ordering_order_placed_v3_btn"), matched: []string{"ordering"}},
		{name: "case and spaces", services: []string{" POS "}, want: with("pos_receipt_share_btn"), matched: []string{"pos"}},
		{name: "treasury maps to finance", services: []string{"treasury"}, want: with("finance_invoice_sent"), matched: []string{"treasury"}},
		{name: "erp", services: []string{"erp"}, want: with("finance_invoice_sent", "hrm_leave_approved", "payroll_payslip_ready"), matched: []string{"erp"}},
		{name: "erp and finance do not duplicate", services: []string{"erp", "finance", "erp"}, want: with("finance_invoice_sent", "hrm_leave_approved", "payroll_payslip_ready"), matched: []string{"erp", "finance"}},
		{name: "hadia from its delivery logs", services: []string{"hadia"}, want: with("hadia_registry_published_v1_btn"), matched: []string{"hadia"}},
		{name: "unknown services ignored", services: []string{"marketflow", "platform", "codevertex"}, want: with(), matched: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, matched := sel.Select(testDefs, tc.services)
			if g := sortStrings(names(got)); !reflect.DeepEqual(g, tc.want) {
				t.Fatalf("selected %v, want %v", g, tc.want)
			}
			if !reflect.DeepEqual(matched, tc.matched) {
				t.Fatalf("matched %v, want %v", matched, tc.matched)
			}
			for _, n := range names(got) {
				if strings.HasPrefix(n, "codevertex_") || strings.HasPrefix(n, "provider_") {
					t.Fatalf("platform-only template %s selected for a tenant", n)
				}
			}
		})
	}
}

// An empty selection must select nothing, not everything (FilterByPrefix treats no prefixes as all).
func TestEmptySelectionSelectsNothing(t *testing.T) {
	got, _ := Selection{}.Select(testDefs, []string{"ordering"})
	if len(got) != 0 {
		t.Fatalf("selected %v from an empty map", names(got))
	}
}

func TestLoadSelectionFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "map.json")
	if err := os.WriteFile(path, []byte(`{"shared":["auth_"],"services":{"Ordering":["ordering_"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SelectionFileEnv, path)
	sel, err := LoadSelection()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := sel.Select(testDefs, []string{"ordering", "pos"})
	if g := sortStrings(names(got)); !reflect.DeepEqual(g, []string{"auth_otp", "ordering_order_placed_v3_btn"}) {
		t.Fatalf("selected %v", g)
	}

	t.Setenv(SelectionFileEnv, filepath.Join(t.TempDir(), "missing.json"))
	if _, err := LoadSelection(); err == nil {
		t.Fatal("a missing override file must be an error, not a silent default")
	}
}

// Every prefix in the default map must select at least one manifest template, so a typo cannot
// quietly drop a service.
func TestDefaultSelectionPrefixesExistInManifest(t *testing.T) {
	sel, err := ParseSelection(defaultSelectionJSON)
	if err != nil {
		t.Fatal(err)
	}
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	all := append([]string{}, sel.Shared...)
	for _, ps := range sel.Services {
		all = append(all, ps...)
	}
	for _, p := range all {
		if len(templatesync.FilterByPrefix(defs, []string{p})) == 0 {
			t.Errorf("prefix %q matches no manifest template", p)
		}
	}
}

func TestServiceFromTemplateID(t *testing.T) {
	cases := map[string]string{
		"ordering/order_placed":      "ordering",
		"subscription/invoice_ready": "subscription",
		"whatsapp/inbox_reply":       "whatsapp",
		"Hadia/gift_receipt":         "hadia",
		"order_confirmation":         "",
		"/x":                         "",
		"":                           "",
	}
	for in, want := range cases {
		if got := ServiceFromTemplateID(in); got != want {
			t.Errorf("ServiceFromTemplateID(%q) = %q, want %q", in, got, want)
		}
	}
}

// A platform owner acting for another tenant carries the platform's entitlements; they must not
// decide that tenant's templates.
func TestClaimsServicesOnlyForOwnTenant(t *testing.T) {
	c := &authclient.Claims{TenantID: "t1", ActiveServiceTags: []string{"ordering"}, ActiveProducts: []string{"pos"}}
	ctx := authclient.ContextWithClaims(context.Background(), c)
	got, _ := ClaimsServices(ctx, "t1")
	if !reflect.DeepEqual(got, []string{"ordering", "pos"}) {
		t.Fatalf("own tenant: %v", got)
	}
	if got, _ := ClaimsServices(ctx, "t2"); len(got) != 0 {
		t.Fatalf("another tenant must get nothing from these claims, got %v", got)
	}
	if got, _ := ClaimsServices(context.Background(), "t1"); len(got) != 0 {
		t.Fatalf("no claims: %v", got)
	}
}
