package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/notifications-api/internal/modules/identity"
	"github.com/bengobox/notifications-api/internal/modules/rbac"
)

// AuthMeHandler returns the current user's service-level profile,
// including notifications-specific roles and permissions from the local RBAC system.
type AuthMeHandler struct {
	rbacService *rbac.Service
	logger      *zap.Logger
	platformID  string
}

// NewAuthMeHandler creates a new auth/me handler.
func NewAuthMeHandler(rbacService *rbac.Service, logger *zap.Logger, platformID string) *AuthMeHandler {
	return &AuthMeHandler{rbacService: rbacService, logger: logger, platformID: platformID}
}

type authMeResponse struct {
	ID              string   `json:"id"`
	Email           string   `json:"email"`
	FullName        string   `json:"fullName"`
	TenantID        string   `json:"tenantId"`
	TenantSlug      string   `json:"tenantSlug"`
	IsPlatformOwner bool     `json:"isPlatformOwner"`
	Roles           []string `json:"roles"`
	Permissions     []string `json:"permissions"`
}

// GetMe returns the authenticated user's service-level profile with local RBAC roles and permissions.
func (h *AuthMeHandler) GetMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get identity user from context (set by authenticator.RequireAuth middleware)
	user, ok := identity.UserFromContext(ctx)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	tenantID := httpware.GetTenantID(ctx)
	tenantSlug := httpware.GetTenantSlug(ctx)
	isPlatformOwner := httpware.IsPlatformOwner(ctx)

	// Start with identity-level roles and permissions
	roles := make([]string, 0, len(user.Roles))
	for _, r := range user.Roles {
		roles = append(roles, string(r))
	}
	permissions := make([]string, 0, len(user.Permissions))
	for _, p := range user.Permissions {
		permissions = append(permissions, string(p))
	}

	// Enrich with service-level RBAC permissions from local DB
	if tenantID != "" {
		tenantUUID, err := uuid.Parse(tenantID)
		if err == nil {
			// Get the auth-service user ID for RBAC lookups
			userID := user.ID
			if user.AuthServiceUserID != nil {
				userID = *user.AuthServiceUserID
			}

			// Fetch service-level roles
			svcRoles, err := h.rbacService.GetUserRoles(ctx, tenantUUID, userID)
			if err == nil {
				for _, sr := range svcRoles {
					roles = append(roles, sr.RoleCode)
				}
			}

			// Fetch service-level permissions
			svcPerms, err := h.rbacService.GetUserPermissions(ctx, tenantUUID, userID)
			if err == nil {
				for _, sp := range svcPerms {
					permissions = append(permissions, sp.PermissionCode)
				}
			}
		}
	}

	// Superuser/platform owner gets all permissions
	if isPlatformOwner || hasRoleStr(roles, "superuser") {
		claims, found := authclient.ClaimsFromContext(ctx)
		if found && claims.GetTenantSlug() == "codevertex" {
			isPlatformOwner = true
		}
		// Add all service permissions for superuser
		for _, p := range identity.AllPermissions() {
			permissions = appendUnique(permissions, string(p))
		}
	}

	resp := authMeResponse{
		ID:              user.ID.String(),
		Email:           user.Email,
		FullName:        user.FullName,
		TenantID:        tenantID,
		TenantSlug:      tenantSlug,
		IsPlatformOwner: isPlatformOwner,
		Roles:           dedupStrings(roles),
		Permissions:     dedupStrings(permissions),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func hasRoleStr(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

func appendUnique(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
