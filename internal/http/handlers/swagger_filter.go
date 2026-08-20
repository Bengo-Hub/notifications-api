package handlers

// filterSpecToTags returns a shallow copy of an OpenAPI/Swagger spec map whose "paths" only
// includes operations tagged with at least one tag in allowed. Every other top-level key
// (definitions, securityDefinitions, info, swagger, host, basePath, schemes) passes through
// unchanged so $ref lookups and the spec's own metadata stay valid for whatever's left. Mirrors
// treasury-api's and auth-api's internal/http(api)/handlers/swagger_filter.go verbatim -- same
// shape of problem, no reason to diverge.
func filterSpecToTags(spec map[string]any, allowed map[string]bool) map[string]any {
	out := make(map[string]any, len(spec))
	for k, v := range spec {
		out[k] = v
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		return out
	}

	filteredPaths := make(map[string]any, len(paths))
	for path, methodsRaw := range paths {
		methods, ok := methodsRaw.(map[string]any)
		if !ok {
			continue
		}
		filteredMethods := make(map[string]any, len(methods))
		for method, opRaw := range methods {
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}
			if operationHasAllowedTag(op, allowed) {
				filteredMethods[method] = op
			}
		}
		if len(filteredMethods) > 0 {
			filteredPaths[path] = filteredMethods
		}
	}
	out["paths"] = filteredPaths
	return out
}

func operationHasAllowedTag(op map[string]any, allowed map[string]bool) bool {
	tagsRaw, ok := op["tags"].([]any)
	if !ok {
		return false
	}
	for _, t := range tagsRaw {
		if tag, ok := t.(string); ok && allowed[tag] {
			return true
		}
	}
	return false
}
