package preferences

import "testing"

// Every template on disk is a configurable notification type, not only the hand-classified ones
// (owner, 20 November 2026: billing/provider_balance_low was missing from the settings page).
func TestCatalogIncludesEveryTemplate(t *testing.T) {
	cat := Catalog([]string{"billing/provider_balance_low", "finance/payment_success", "system_test", "billing/provider_balance_low"})
	byKey := map[string]Type{}
	for _, ty := range cat {
		if _, dup := byKey[ty.Key]; dup {
			t.Fatalf("duplicate catalog key %q", ty.Key)
		}
		byKey[ty.Key] = ty
	}
	got, ok := byKey["billing/provider_balance_low"]
	if !ok {
		t.Fatal("billing/provider_balance_low is not configurable")
	}
	if got.Class != ClassEssential || got.Label != "Provider balance low" || got.Group != "Billing" {
		t.Fatalf("unregistered template = %+v, want essential, labelled from its id", got)
	}
	if _, ok := byKey["system_test"]; ok {
		t.Fatal("the system test template is not a notification type")
	}
	if byKey["finance/payment_success"].Label != "Payment successful" {
		t.Fatal("a registered type keeps its registry label")
	}
	if len(cat) < len(Registry) {
		t.Fatalf("catalog has %d types, fewer than the registry's %d", len(cat), len(Registry))
	}
	if _, ok := LookupIn(cat, "billing/provider_balance_low"); !ok {
		t.Fatal("LookupIn misses a discovered type")
	}
}

func TestNoChannelsMarker(t *testing.T) {
	if channelInList(NoChannels, "email") {
		t.Fatal("the no-channels marker must match no channel")
	}
	if got := ChannelsValue(nil); got != NoChannels {
		t.Fatalf("ChannelsValue(nil) = %q, want %q", got, NoChannels)
	}
	if got := ChannelsValue([]string{"email", "whatsapp"}); got != "email,whatsapp" {
		t.Fatalf("ChannelsValue = %q", got)
	}
}
