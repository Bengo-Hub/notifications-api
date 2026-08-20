package handlers

import (
	_ "embed"
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

//go:embed swagger.json
var swaggerSpecBytes []byte

// externalDocTags are the only tags shown to a caller who isn't holding a genuine Codevertex
// platform App secret -- an anonymous visitor, or anyone with an invalid/non-platform secret.
// Everything else in the spec (Analytics, Templates, Platform provider config) is internal/
// tenant-operational surface, not something an external integrator sending notifications needs.
var externalDocTags = map[string]bool{
	"Notifications": true,
	"Health":        true,
}

// SwaggerHandler serves the OpenAPI spec and its Swagger UI. The embedded spec is parsed once at
// startup; the external-only view is pre-computed at the same time so every request is a cheap
// map lookup, never a re-parse. Mirrors treasury-api's internal/http/handlers/swagger.go and
// auth-api's internal/httpapi/handlers/swagger_handler.go -- same shape of problem, no reason to
// diverge. Swagger UI 5 renders Swagger 2.0 specs natively, so unlike this file's previous
// version, there's no OpenAPI-3 conversion step (which was also where the stale/typo'd production
// host got hardcoded) -- the spec's own real `host` field is served as-is.
type SwaggerHandler struct {
	apiKeyValidator *authclient.APIKeyValidator
	fullSpec        map[string]any
	externalSpec    map[string]any
}

// NewSwaggerHandler builds the handler from the embedded swagger.json. apiKeyValidator may be nil
// (e.g. in a test binary that doesn't wire one) -- OpenAPIJSON simply always serves the
// external-only view in that case, never the internal one.
func NewSwaggerHandler(apiKeyValidator *authclient.APIKeyValidator) (*SwaggerHandler, error) {
	var full map[string]any
	if err := json.Unmarshal(swaggerSpecBytes, &full); err != nil {
		return nil, err
	}
	return &SwaggerHandler{
		apiKeyValidator: apiKeyValidator,
		fullSpec:        full,
		externalSpec:    filterSpecToTags(full, externalDocTags),
	}, nil
}

// isPrivilegedForInternalDocs reports whether a validated app secret belongs to a genuine
// platform-type App (roles contains "superuser") as opposed to a tenant-scoped App or a
// bare/invalid secret.
func isPrivilegedForInternalDocs(result *authclient.APIKeyValidationResult) bool {
	if result == nil {
		return false
	}
	return result.ToClaims().IsPlatformOwner
}

func writeSwaggerCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
	w.Header().Set("Access-Control-Expose-Headers", "X-Docs-View, X-Docs-Environment")
	w.Header().Set("Access-Control-Max-Age", "3600")
}

// resolveAppSecretOptional validates the X-API-Key header (a pasted App secret) against auth-api's
// existing public key-validation endpoint and returns the result, or nil if there's no header or
// it doesn't validate. Never rejects the request -- an invalid/missing secret here just means
// "anonymous," not "reject," since OpenAPIJSON always has a safe (external-only) response to fall
// back to.
func resolveAppSecretOptional(validator *authclient.APIKeyValidator, r *http.Request) *authclient.APIKeyValidationResult {
	secret := r.Header.Get("X-API-Key")
	if validator == nil || secret == "" {
		return nil
	}
	result, err := validator.ValidateAPIKeyFull(r.Context(), secret)
	if err != nil {
		return nil
	}
	return result
}

// OpenAPIJSON serves the OpenAPI/Swagger JSON specification: the full internal spec for a
// resolved platform App secret, the external-only subset for everyone else (anonymous visitors,
// tenant App/APIKey holders, and invalid secrets). Also reports the resolved view and the
// credential's environment (sandbox/production) via response headers, so the docs UI's
// sandbox/production badge reflects the same server-side decision instead of re-deriving it.
func (h *SwaggerHandler) OpenAPIJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeSwaggerCORSHeaders(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeSwaggerCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")

	resolved := resolveAppSecretOptional(h.apiKeyValidator, r)
	spec := h.externalSpec
	docsView := "external"
	environment := "none"
	if resolved != nil {
		environment = resolved.Environment
		if isPrivilegedForInternalDocs(resolved) {
			spec = h.fullSpec
			docsView = "internal"
		}
	}
	w.Header().Set("X-Docs-View", docsView)
	w.Header().Set("X-Docs-Environment", environment)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(spec)
}

// SwaggerUI serves the Swagger UI HTML page. See renderDocsHTML's doc comment for the app-secret
// unlock contract this page implements -- identical across treasury-api, auth-api, and
// notifications-api.
func (h *SwaggerHandler) SwaggerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(renderDocsHTML("Notifications Service API Docs"))
}

// renderDocsHTML builds the fleet-standard docs page: an app-secret ("bng_app_..." or a plain
// developer "bng_..." key) paste bar that drives two independent, server-verified unlocks --
// internal/full spec visibility (platform secrets only) and a sandbox/production badge (reflects
// the secret's real Environment) -- via the same X-API-Key header on both the spec fetch and every
// "Try it out" call. No credential ever unlocks anything client-side; OpenAPIJSON is the sole
// source of truth and reports its decision back via X-Docs-View/X-Docs-Environment headers.
func renderDocsHTML(title string) []byte {
	return []byte(`<!DOCTYPE html>
<html>
  <head>
    <meta charset="UTF-8">
    <title>` + title + `</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
    <style>
      #docs-token-bar {
        display: flex; flex-wrap: wrap; gap: 8px; align-items: center;
        padding: 10px 16px; background: #1b1b1b; color: #fff; font: 13px sans-serif;
      }
      #docs-token-bar input {
        flex: 1; min-width: 240px; max-width: 420px; padding: 6px 8px;
        border-radius: 4px; border: 1px solid #444; background: #111; color: #fff;
      }
      #docs-token-bar button {
        padding: 6px 14px; border-radius: 4px; border: none;
        background: #61affe; color: #0b1620; cursor: pointer; font-weight: 600;
      }
      #docs-token-bar span.hint { opacity: .75; }
      #docs-view-badge, #docs-env-badge {
        font-weight: 600; padding: 3px 10px; border-radius: 12px; font-size: 12px;
      }
      #docs-view-badge { background: #333; }
      #docs-env-badge.sandbox { background: #614a00; color: #ffd76b; }
      #docs-env-badge.production { background: #0b4a1f; color: #6bffa0; }
    </style>
  </head>
  <body>
    <div id="docs-token-bar">
      <span class="hint">Codevertex staff or developers with an app secret:</span>
      <input id="docs-token-input" type="password" placeholder="Paste your app secret (bng_app_... or bng_...)" />
      <button id="docs-token-apply">Unlock</button>
      <button id="docs-token-clear">Clear</button>
      <span id="docs-view-badge">External view</span>
      <span id="docs-env-badge" class="sandbox">Sandbox</span>
    </div>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
    <script>
      const SECRET_KEY = 'codevertex_docs_app_secret';
      const specUrl = window.location.protocol + '//' + window.location.host + '/api/v1/openapi.json';

      function requestInterceptor(request) {
        // Carries the same pasted app secret onto every "Try it out" call as X-API-Key, so
        // testing a real request exercises the same sandbox/production credential distinction
        // enforced server-side -- unrelated to Swagger UI's own Authorize dialog, if the spec
        // also declares other security schemes.
        const secret = localStorage.getItem(SECRET_KEY);
        if (secret && request.headers && !request.headers['X-API-Key']) {
          request.headers['X-API-Key'] = secret;
        }
        return request;
      }

      function setBadges(docsView, environment) {
        const viewBadge = document.getElementById('docs-view-badge');
        viewBadge.textContent = docsView === 'internal' ? 'Internal view' : 'External view';
        const envBadge = document.getElementById('docs-env-badge');
        const env = environment === 'production' ? 'production' : 'sandbox';
        envBadge.textContent = env === 'production' ? 'Production' : 'Sandbox';
        envBadge.className = env;
      }

      function renderDocs(secret) {
        const headers = secret ? { 'X-API-Key': secret } : {};
        fetch(specUrl, { headers: headers })
          .then((res) => {
            setBadges(res.headers.get('X-Docs-View'), res.headers.get('X-Docs-Environment'));
            return res.json();
          })
          .then((spec) => {
            window.ui = SwaggerUIBundle({
              spec: spec,
              dom_id: '#swagger-ui',
              presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
              layout: 'BaseLayout',
              deepLinking: true,
              filter: true,
              persistAuthorization: true,
              requestInterceptor: requestInterceptor,
            });
          });
      }

      window.onload = () => {
        const saved = localStorage.getItem(SECRET_KEY) || '';
        document.getElementById('docs-token-input').value = saved;
        renderDocs(saved);

        document.getElementById('docs-token-apply').addEventListener('click', () => {
          const secret = document.getElementById('docs-token-input').value.trim();
          localStorage.setItem(SECRET_KEY, secret);
          renderDocs(secret);
        });
        document.getElementById('docs-token-clear').addEventListener('click', () => {
          localStorage.removeItem(SECRET_KEY);
          document.getElementById('docs-token-input').value = '';
          renderDocs('');
        });
      }
    </script>
  </body>
</html>`)
}
