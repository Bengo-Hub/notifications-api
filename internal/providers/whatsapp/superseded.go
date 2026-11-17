package whatsapp

// supersededTemplate names the approved template a rewritten one replaces. order lists, for each
// of the old template's {{n}}, the index of the new template's parameter that fills it; nil keeps
// the new order, and an empty slice sends no parameters.
type supersededTemplate struct {
	old   string
	order []int
}

// superseded maps a rewritten template to the approved one it replaces, while Meta reviews the
// rewrite: its sends fall back to the old template (template_fallback_name), so a new wording can
// ship before approval. Drop an entry once its rewrite is approved on every WhatsApp account we send
// from (GET /platform/whatsapp/templates/categories).
//
// The entries of 17 November 2026 are the policy audit's rewrites of approved templates with words
// Meta reads as marketing or a login ("enjoy", "thank you for choosing", "sign in"); same parameters,
// same buttons. projects_client_invited_v2_btn applies once cmd/worker/projects_portal.go sends it.
var superseded = map[string]supersededTemplate{
	"ordering_order_delivered_v5_btn": {old: "ordering_order_delivered_v3_btn"},
	"ordering_order_delivered_v6_btn": {old: "ordering_order_delivered_v4_btn"},
	"ordering_order_completed_v3_btn": {old: "ordering_order_completed_v1_btn"},
	"ordering_order_completed_v4_btn": {old: "ordering_order_completed_v2_btn"},
	"projects_client_invited_v2_btn":  {old: "projects_client_invited_v1_btn"},
}

// addSupersededFallback sets the fallback for a rewritten template. A fallback the caller set is
// kept, unless it is itself a rewrite still in review: then the approved template the primary
// replaces is the safer fallback (an order's two-button review template falls back to its own
// approved predecessor, with both buttons, rather than to the new one-button template).
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
		if _, inReview := superseded[fb]; !inReview {
			return
		}
		delete(metadata, "template_fallback_button_param")
		delete(metadata, "template_fallback_button_params")
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
	if bs := stringSliceParam(metadata["template_button_params"]); len(bs) > 0 {
		metadata["template_fallback_button_params"] = append([]string(nil), bs...)
	}
}
