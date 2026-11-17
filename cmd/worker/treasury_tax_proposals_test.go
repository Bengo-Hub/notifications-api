package main

import (
	"strings"
	"testing"
)

func TestTaxProposalsData(t *testing.T) {
	t.Setenv("NOTIFICATIONS_TREASURY_APP_URL", "")
	d := taxProposalsData(map[string]any{"count": float64(2), "codes": []any{"WHT_GOODS_PUBLIC", " ", "VAT_STANDARD"}})
	if d["count"] != 2 || d["codes"] != "WHT_GOODS_PUBLIC, VAT_STANDARD" {
		t.Fatalf("data = %v", d)
	}
	if d["review_link"] != "https://books.codevertexafrica.com/codevertex/tax/advisor?section=updates" {
		t.Fatalf("review_link = %v", d["review_link"])
	}

	many := make([]any, 13)
	for i := range many {
		many[i] = "C" + strings.Repeat("X", i+1)
	}
	if got := taxProposalsData(map[string]any{"codes": many})["codes"].(string); !strings.HasSuffix(got, "and 3 more") {
		t.Fatalf("long code lists are shortened, got %q", got)
	}
}

func TestPlatformAlertRecipients(t *testing.T) {
	got := platformAlertRecipients(" ops@codevertexafrica.com, OPS@codevertexafrica.com,,not-an-email, owner@example.com ")
	if len(got) != 2 || got[0] != "ops@codevertexafrica.com" || got[1] != "owner@example.com" {
		t.Fatalf("got %v", got)
	}
	if len(platformAlertRecipients("")) != 0 {
		t.Fatal("an empty list sends nothing")
	}
}

func TestTaxProposalsMappingRegistered(t *testing.T) {
	m, ok := treasuryMappings[taxProposalsPendingEvent]
	if !ok || m.TemplateID != "finance/tax_proposals_pending" {
		t.Fatalf("mapping missing: %+v", m)
	}
}
