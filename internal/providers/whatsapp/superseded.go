package whatsapp

// supersededTemplate names the approved template a rewritten one replaces. order lists, for each
// of the old template's {{n}}, the index of the new template's parameter that fills it; nil keeps
// the new order, and an empty slice sends no parameters.
type supersededTemplate struct {
	old   string
	order []int
}

// superseded maps each rewritten template to the one it replaces (16 November 2026). Meta had
// approved the old ones as MARKETING although we submitted them as UTILITY, so they were billed at
// the marketing rate and held back by Meta's per person marketing limit (131049). The rewrites go
// to Meta under new names; until Meta approves one, its sends fall back to the old template, which
// is approved and still works (template_fallback_name). Drop an entry once its rewrite is approved
// on every WhatsApp account we send from (GET /platform/whatsapp/templates/categories).
var superseded = map[string]supersededTemplate{
	"notifications_test_message_v2":   {old: "notifications_test_message_v1", order: []int{}},
	"credits_balance_low_v2_btn":      {old: "credits_balance_low_v1_btn"},
	"hadia_registry_published_v2_btn": {old: "hadia_registry_published_v1_btn"},
	"hadia_legal_updated_v2_btn":      {old: "hadia_legal_updated_v1_btn"},
	"pos_loyalty_referral_v2":         {old: "pos_loyalty_referral_v1"},
	// v2 asks for (name, registry, co_owner, change); v1 took (name, co_owner, change, registry).
	"hadia_change_requested_v2_btn": {old: "hadia_change_requested_v1_btn", order: []int{0, 2, 3, 1}},
}

// addSupersededFallback sets the fallback for a rewritten template that has none of its own.
func addSupersededFallback(metadata map[string]interface{}) {
	if metadata == nil {
		return
	}
	name, _ := metadata["template_name"].(string)
	s, ok := superseded[name]
	if !ok {
		return
	}
	if fb, _ := metadata["template_fallback_name"].(string); fb != "" {
		return
	}
	params := stringSliceParam(metadata["template_params"])
	var fbParams []string
	switch {
	case s.order == nil:
		fbParams = append([]string(nil), params...)
	default:
		fbParams = make([]string, 0, len(s.order))
		for _, i := range s.order {
			if i < len(params) {
				fbParams = append(fbParams, params[i])
			}
		}
	}
	metadata["template_fallback_name"] = s.old
	metadata["template_fallback_params"] = fbParams
	if b, _ := metadata["template_button_param"].(string); b != "" {
		metadata["template_fallback_button_param"] = b
	}
}
