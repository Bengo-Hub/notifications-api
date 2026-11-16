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
// Empty since 17 November 2026: the six utility rewrites of templates Meta had billed as marketing
// (test message, credit balance, Hadia registry published, change requested and legal notices, POS
// referral points) were all approved as UTILITY within an hour, and their entries were removed.
var superseded = map[string]supersededTemplate{}

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
