package handlers

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed swagger.json
var swaggerSpec []byte

// convertRefs recursively converts Swagger 2.0 $ref paths to OpenAPI 3.0 format
// Changes #/definitions/... to #/components/schemas/...
func convertRefs(data interface{}) interface{} {
	switch v := data.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for key, value := range v {
			if key == "$ref" {
				if refStr, ok := value.(string); ok {
					// Convert #/definitions/... to #/components/schemas/...
					if strings.HasPrefix(refStr, "#/definitions/") {
						value = strings.Replace(refStr, "#/definitions/", "#/components/schemas/", 1)
					}
				}
				result[key] = value
			} else {
				result[key] = convertRefs(value)
			}
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, item := range v {
			result[i] = convertRefs(item)
		}
		return result
	default:
		return v
	}
}

// convertSwagger2ToOpenAPI3 converts Swagger 2.0 spec to OpenAPI 3.0 format
func convertSwagger2ToOpenAPI3(swagger2Spec map[string]interface{}) map[string]interface{} {
	openAPI3 := make(map[string]interface{})

	// Set OpenAPI version
	openAPI3["openapi"] = "3.0.3"

	// Copy info
	if info, ok := swagger2Spec["info"].(map[string]interface{}); ok {
		openAPI3["info"] = info
	}

	// Convert servers - add both local and production
	openAPI3["servers"] = []map[string]interface{}{
		{
			"url":         "http://notifications.codevertex.local:4002",
			"description": "Local development (HTTP)",
		},
		{
			"url":         "https://notifications.codevertex.local:4002",
			"description": "Local development (HTTPS)",
		},
		{
			"url":         "https://notifications.codevrtexitsolutions.com",
			"description": "Production",
		},
	}

	// Copy components (security schemes, schemas)
	components := make(map[string]interface{})

	// Convert security definitions to security schemes
	if securityDefs, ok := swagger2Spec["securityDefinitions"].(map[string]interface{}); ok {
		securitySchemes := make(map[string]interface{})
		for key, value := range securityDefs {
			securitySchemes[key] = value
		}
		components["securitySchemes"] = securitySchemes
	}

	// Copy definitions to schemas
	if definitions, ok := swagger2Spec["definitions"].(map[string]interface{}); ok {
		components["schemas"] = definitions
	}

	if len(components) > 0 {
		openAPI3["components"] = components
	}

	// Copy security
	if security, ok := swagger2Spec["security"].([]interface{}); ok {
		openAPI3["security"] = security
	}

	// Convert paths - all paths should use /api/v1 prefix
	// In Swagger 2.0, paths are relative to basePath
	// In OpenAPI 3.0, paths are absolute
	basePath := "/api/v1"
	if bp, ok := swagger2Spec["basePath"].(string); ok && bp != "" {
		basePath = strings.TrimSuffix(bp, "/")
	}

	if paths, ok := swagger2Spec["paths"].(map[string]interface{}); ok {
		convertedPaths := make(map[string]interface{})
		for path, pathItem := range paths {
			// Ensure path starts with /
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}

			// Prepend basePath to all paths if not already present
			if basePath != "" && !strings.HasPrefix(path, basePath) {
				path = basePath + path
			}
			// Convert $ref references in path items
			convertedPaths[path] = convertRefs(pathItem)
		}
		openAPI3["paths"] = convertedPaths
	}

	// Convert $ref references in components/schemas as well
	if components, ok := openAPI3["components"].(map[string]interface{}); ok {
		if schemas, ok := components["schemas"].(map[string]interface{}); ok {
			components["schemas"] = convertRefs(schemas)
		}
		openAPI3["components"] = components
	}

	return openAPI3
}

// OpenAPIJSON serves the OpenAPI/Swagger JSON specification
func OpenAPIJSON(w http.ResponseWriter, r *http.Request) {
	// Handle OPTIONS preflight requests
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
		w.Header().Set("Access-Control-Max-Age", "3600")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Parse the Swagger spec
	var spec map[string]interface{}
	if err := json.Unmarshal(swaggerSpec, &spec); err == nil {
		// Check if it's Swagger 2.0 and convert to OpenAPI 3.0
		if swaggerVersion, ok := spec["swagger"].(string); ok && strings.HasPrefix(swaggerVersion, "2.") {
			openAPI3Spec := convertSwagger2ToOpenAPI3(spec)
			if modifiedSpec, err := json.Marshal(openAPI3Spec); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
				w.Write(modifiedSpec)
				return
			}
		} else {
			// Already OpenAPI 3.0, just ensure servers are set
			if _, hasServers := spec["servers"]; !hasServers {
				spec["servers"] = []map[string]interface{}{
					{"url": "http://notifications.codevertex.local:4002", "description": "Local development (HTTP)"},
					{"url": "https://notifications.codevertex.local:4002", "description": "Local development (HTTPS)"},
					{"url": "https://notifications.codevrtexitsolutions.com", "description": "Production"},
				}
			}
			if modifiedSpec, err := json.Marshal(spec); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
				w.Write(modifiedSpec)
				return
			}
		}
	}

	// Fallback to original spec if modification fails
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
	w.Write(swaggerSpec)
}

// SwaggerUI serves the Swagger UI HTML page
func SwaggerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	w.Write([]byte(`<!DOCTYPE html>
<html>
  <head>
    <meta charset="UTF-8">
    <title>Notifications Service API Docs</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
    <script>
      window.onload = () => {
        // Use the same protocol as the current page to avoid HTTP/HTTPS mismatch
        const specUrl = window.location.protocol + '//' + window.location.host + '/api/v1/openapi.json';
        window.ui = SwaggerUIBundle({
          url: specUrl,
          dom_id: '#swagger-ui',
          presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
          layout: "BaseLayout",
          deepLinking: true,
          filter: true,
          persistAuthorization: true,
          tryItOutEnabled: true,
          requestInterceptor: (request) => {
            // Ensure CORS headers are included and use proper URL scheme
            request.credentials = 'omit';
            // Ensure request URL uses http or https scheme
            if (request.url && !request.url.match(/^https?:\/\//)) {
              // If relative URL, make it absolute using current origin
              request.url = window.location.origin + request.url;
            }
            // Automatically add "Bearer " prefix to Authorization header if missing
            if (request.headers && request.headers.Authorization) {
              const authHeader = request.headers.Authorization;
              // Check if it doesn't already start with "Bearer " (case-insensitive)
              if (!/^bearer\s+/i.test(authHeader)) {
                request.headers.Authorization = 'Bearer ' + authHeader.trim();
              }
            }
            return request;
          },
          responseInterceptor: (response) => {
            return response;
          }
        })
      }
    </script>
  </body>
</html>`))
}
