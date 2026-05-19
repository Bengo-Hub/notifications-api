package identityhandler

import (
	"net/http"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/http/handlers"
	"github.com/bengobox/notifications-api/internal/modules/identity"
)

// Authenticator provides middleware helpers for RBAC-protected routes.
type Authenticator struct {
	log           *zap.Logger
	service       *identity.Service
	authValidator *authclient.Validator
}

// NewAuthenticator constructs middleware helpers.
func NewAuthenticator(log *zap.Logger, service *identity.Service, authValidator *authclient.Validator) *Authenticator {
	return &Authenticator{
		log:           log.Named("identity.Authenticator"),
		service:       service,
		authValidator: authValidator,
	}
}

// IsSuperuser checks if the user is a superuser from auth-service.
func IsSuperuser(claims *authclient.Claims) bool {
	if claims == nil {
		return false
	}
	return claims.IsSuperuser()
}

// IsAdmin checks if the user has admin or superuser role from auth-service.
func IsAdmin(claims *authclient.Claims) bool {
	if claims == nil {
		return false
	}
	return claims.IsAdmin()
}

// claimsHasPermission checks if the JWT claims contain a specific permission code.
func claimsHasPermission(claims *authclient.Claims, perm identity.Permission) bool {
	if claims == nil {
		return false
	}
	permStr := string(perm)
	for _, p := range claims.Permissions {
		if p == permStr {
			return true
		}
	}
	return false
}

// claimsHasAllPermissions checks if the JWT claims contain all specified permission codes.
func claimsHasAllPermissions(claims *authclient.Claims, perms []identity.Permission) bool {
	for _, perm := range perms {
		if !claimsHasPermission(claims, perm) {
			return false
		}
	}
	return true
}

// RequireAuth enforces presence of a valid access token.
// Uses auth-service JWT validation, loads/JIT-provisions user into context.
// Bypass: if an API key already placed superuser/admin claims in context (S2S calls),
// skip the JWT+local-user requirement since permissions are checked downstream.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if existingClaims, ok := authclient.ClaimsFromContext(r.Context()); ok && existingClaims != nil {
			if IsSuperuser(existingClaims) || IsAdmin(existingClaims) {
				next.ServeHTTP(w, r)
				return
			}
		}

		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			handlers.RespondError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		if a.authValidator == nil {
			handlers.RespondError(w, http.StatusUnauthorized, "auth-service validator not configured")
			return
		}

		authClaims, err := a.authValidator.ValidateToken(token)
		if err != nil {
			a.log.Warn("auth-service token validation failed", zap.Error(err))
			handlers.RespondError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		userID, err := authClaims.UserID()
		if err != nil {
			handlers.RespondError(w, http.StatusUnauthorized, "invalid user ID in token")
			return
		}

		// Load or JIT-provision user from local database
		user, err := a.service.GetUser(r.Context(), userID)
		if err != nil {
			// JIT provisioning: valid token but user not in DB
			tenantSlug := authClaims.GetTenantSlug()
			authUserData := map[string]interface{}{"roles": authClaims.Roles, "permissions": authClaims.Permissions}
			if authClaims.Email != "" {
				authUserData["email"] = authClaims.Email
			}
			user, err = a.service.EnsureUserFromToken(r.Context(), userID, tenantSlug, authUserData)
			if err != nil {
				a.log.Warn("JIT provision failed", zap.String("user_id", userID.String()), zap.Error(err))
				handlers.RespondError(w, http.StatusUnauthorized, "user not found")
				return
			}
			a.log.Info("user JIT-provisioned", zap.String("user_id", userID.String()))
		}

		ctx := identity.ContextWithUser(r.Context(), user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OptionalAuth attempts to authenticate the user but does not fail if missing or invalid.
func (a *Authenticator) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}

		if a.authValidator != nil {
			authClaims, err := a.authValidator.ValidateToken(token)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			userID, err := authClaims.UserID()
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			user, err := a.service.GetUser(r.Context(), userID)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			ctx := identity.ContextWithUser(r.Context(), user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// RequireRoles enforces that the authenticated user has at least one of the supplied roles.
// Superusers and admins bypass this check.
func (a *Authenticator) RequireRoles(roles ...identity.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check superuser/admin from auth-service JWT claims first
			if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
				if IsSuperuser(claims) || IsAdmin(claims) {
					next.ServeHTTP(w, r)
					return
				}
				for _, required := range roles {
					if claims.HasRole(string(required)) {
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			// Fall back to local user roles
			user, ok := identity.UserFromContext(r.Context())
			if !ok {
				handlers.RespondError(w, http.StatusForbidden, "user missing from context")
				return
			}
			if !userHasRole(user, roles) {
				handlers.RespondError(w, http.StatusForbidden, "insufficient role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePermissions enforces that the user has all supplied permissions.
// Priority: (1) superuser/admin bypass → (2) JWT claims permissions → (3) local DB user permissions.
func (a *Authenticator) RequirePermissions(perms ...identity.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
				if IsSuperuser(claims) || IsAdmin(claims) {
					next.ServeHTTP(w, r)
					return
				}
				if len(claims.Permissions) > 0 {
					if claimsHasAllPermissions(claims, perms) {
						next.ServeHTTP(w, r)
						return
					}
					handlers.RespondError(w, http.StatusForbidden, "insufficient permissions")
					return
				}
			}

			// Fall back to local DB user permissions
			user, ok := identity.UserFromContext(r.Context())
			if !ok {
				handlers.RespondError(w, http.StatusForbidden, "user missing from context")
				return
			}
			if !userHasPermissions(user, perms) {
				handlers.RespondError(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func userHasRole(user *identity.User, roles []identity.Role) bool {
	if len(roles) == 0 {
		return true
	}
	for _, candidate := range roles {
		if user.HasRole(candidate) {
			return true
		}
	}
	return false
}

func userHasPermissions(user *identity.User, perms []identity.Permission) bool {
	if len(perms) == 0 {
		return true
	}
	for _, perm := range perms {
		if !user.HasPermission(perm) {
			return false
		}
	}
	return true
}
