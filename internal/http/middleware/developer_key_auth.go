package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	httpware "github.com/Bengo-Hub/httpware"
	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"go.uber.org/zap"
)

// DeveloperKeyAuth validates an external developer's X-API-Key (bng_* API key or
// bng_app_* App token) against auth-api's existing key-validation endpoint — the
// same one already used by treasury-api's ExternalAPIKeyAuth for eTIMS. Unlike that
// one, this also carries the resolved environment (sandbox/production) into request
// context, since that's what lets Enqueue decide whether to actually touch a real
// provider/production data or the ephemeral sandbox store.
type DeveloperKeyAuth struct {
	client *serviceclient.Client
	log    *zap.Logger
}

func NewDeveloperKeyAuth(authServiceURL string, log *zap.Logger) *DeveloperKeyAuth {
	cfg := serviceclient.DefaultConfig(strings.TrimRight(authServiceURL, "/"), "notifications-api", log.Named("developer-key-auth"))
	cfg.Timeout = 5 * time.Second
	return &DeveloperKeyAuth{client: serviceclient.New(cfg), log: log}
}

type validateKeyResponse struct {
	ClientID    string   `json:"client_id"`
	TenantID    string   `json:"tenant_id"`
	TenantSlug  string   `json:"tenant_slug"`
	Scopes      []string `json:"scopes"`
	Roles       []string `json:"roles"`
	Service     string   `json:"service"`
	Environment string   `json:"environment"`
}

type envContextKey struct{}
type developerKeyContextKey struct{}

// WithEnvironment stashes the resolved environment ("sandbox" or "production") on ctx.
func WithEnvironment(ctx context.Context, env string) context.Context {
	return context.WithValue(ctx, envContextKey{}, env)
}

// EnvironmentFromContext reads it back. Anything not explicitly "sandbox" is treated
// as production — the default for JWT sessions and the internal S2S key, neither of
// which has ever had a sandbox concept, must never accidentally start acting like a
// sandbox call just because this key is unset.
func EnvironmentFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(envContextKey{}).(string); ok && v == "sandbox" {
		return "sandbox"
	}
	return "production"
}

// TryDeveloperKey inspects the X-API-Key header.
//   - No header, or a header not shaped like a developer key (bng_*): returns
//     (nil, false) — "not my concern," the caller should fall through to the
//     existing JWT/internal-key checks completely unchanged.
//   - A bng_*-shaped header that fails validation: writes the 401 response itself
//     and returns (nil, true) — the caller must stop and not process the request
//     any further.
//   - A valid developer key: returns (ctx, true) with tenant + environment stashed
//     on ctx — the caller should proceed using this ctx.
func (a *DeveloperKeyAuth) TryDeveloperKey(w http.ResponseWriter, r *http.Request) (ctx context.Context, isDeveloperKey bool) {
	key := r.Header.Get("X-API-Key")
	if !strings.HasPrefix(key, "bng_") {
		return nil, false
	}

	resp, err := a.client.Get(r.Context(), "/api/v1/admin/api-keys/validate", map[string]string{"X-API-Key": key})
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		http.Error(w, `{"error":"invalid or expired API key"}`, http.StatusUnauthorized)
		return nil, true
	}
	var validated validateKeyResponse
	if decErr := resp.DecodeJSON(&validated); decErr != nil || validated.TenantID == "" {
		a.log.Warn("developer key auth: could not decode key validation response", zap.Error(decErr))
		http.Error(w, `{"error":"invalid or expired API key"}`, http.StatusUnauthorized)
		return nil, true
	}

	out := httpware.WithTenantID(r.Context(), validated.TenantID)
	out = WithEnvironment(out, validated.Environment)
	out = context.WithValue(out, developerKeyContextKey{}, true)
	return out, true
}

// IsDeveloperKeyRequest reports whether this request was already authenticated via a
// bng_*/bng_app_* developer key. The Layer-3 identity/JIT-provisioning middleware
// checks this to skip its own Bearer-token requirement — a developer key authenticates
// an App/tenant, not an individual human user, so there's no local user record to
// JIT-provision here.
func IsDeveloperKeyRequest(ctx context.Context) bool {
	v, _ := ctx.Value(developerKeyContextKey{}).(bool)
	return v
}
