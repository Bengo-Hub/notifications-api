package identity

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/tenant"
)

// Service coordinates identity workflows across persistence and token services.
// Authentication is handled entirely by the SSO (auth-service) via OIDC.
// This service manages local user records synced from auth-service via NATS events.
type Service struct {
	repo         Repository
	logger       *zap.Logger
	now          func() time.Time
	tenantSyncer *tenant.Syncer
}

// NewService constructs the identity service with provided dependencies.
func NewService(repo Repository, logger *zap.Logger, tenantSyncer *tenant.Syncer) *Service {
	return &Service{
		repo:         repo,
		logger:       logger.Named("identity.Service"),
		now:          time.Now,
		tenantSyncer: tenantSyncer,
	}
}

// SyncUserFromAuthService syncs user data from auth-service to local database.
func (s *Service) SyncUserFromAuthService(ctx context.Context, authServiceUserID uuid.UUID, tenantID string, authUserData map[string]interface{}) (*User, error) {
	return s.syncUserFromAuthService(ctx, authServiceUserID, tenantID, authUserData)
}

// EnsureUserFromToken (JIT provisioning): returns the local user for the given auth-service user ID,
// creating a minimal user from token claims if not found.
func (s *Service) EnsureUserFromToken(ctx context.Context, authServiceUserID uuid.UUID, tenantIDOrSlug string, authUserData map[string]interface{}) (*User, error) {
	user, err := s.repo.FindUserByAuthServiceID(ctx, authServiceUserID)
	if err == nil && user != nil {
		return user, nil
	}
	tenantID := tenantIDOrSlug
	if tenantIDOrSlug != "" {
		if _, err := uuid.Parse(tenantIDOrSlug); err != nil {
			// JIT Sync Tenant before we hit DB for it.
			if s.tenantSyncer != nil {
				realTenantID, syncErr := s.tenantSyncer.SyncTenant(ctx, tenantIDOrSlug)
				if syncErr != nil {
					s.logger.Warn("tenant sync failed during JIT user provisioning", zap.Error(syncErr))
				} else {
					tenantID = realTenantID.String()
				}
			}

			// If tenantID is still the slug, resolve from local DB
			if tenantID == tenantIDOrSlug {
				t, err := s.repo.FindTenantBySlug(ctx, tenantIDOrSlug)
				if err != nil {
					return nil, fmt.Errorf("identity: resolve tenant for JIT: %w", err)
				}
				tenantID = t.ID.String()
			}
		}
	}
	if authUserData == nil {
		authUserData = map[string]interface{}{}
	}
	return s.createUserFromAuthService(ctx, authServiceUserID, tenantID, authUserData)
}

// GetUser returns a user by identifier.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.repo.FindUserByID(ctx, id)
}

func (s *Service) syncUserFromAuthService(ctx context.Context, authServiceUserID uuid.UUID, tenantID string, authUserData map[string]interface{}) (*User, error) {
	// Try to find existing user by auth_service_user_id
	user, err := s.repo.FindUserByAuthServiceID(ctx, authServiceUserID)
	if err == nil && user != nil {
		return s.updateUserFromAuthService(ctx, user, authUserData)
	}

	// Try to find by email
	email, _ := authUserData["email"].(string)
	if email != "" {
		user, err = s.repo.FindUserByEmail(ctx, email)
		if err == nil && user != nil {
			user.AuthServiceUserID = &authServiceUserID
			now := s.now()
			user.SyncAt = &now
			user.SyncStatus = "synced"
			if err := s.repo.UpdateUser(ctx, user); err != nil {
				return nil, fmt.Errorf("identity: update user with auth-service ID: %w", err)
			}
			return s.updateUserFromAuthService(ctx, user, authUserData)
		}
	}

	return s.createUserFromAuthService(ctx, authServiceUserID, tenantID, authUserData)
}

func (s *Service) updateUserFromAuthService(ctx context.Context, user *User, authUserData map[string]interface{}) (*User, error) {
	if email, ok := authUserData["email"].(string); ok && email != "" {
		user.Email = email
	}
	if fullName, ok := authUserData["full_name"].(string); ok && fullName != "" {
		user.FullName = fullName
	}
	if phone, ok := authUserData["phone"].(string); ok {
		user.Phone = phone
	}
	if status, ok := authUserData["status"].(string); ok {
		user.Status = status
	}
	if verified, ok := authUserData["email_verified"].(bool); ok {
		user.EmailVerified = verified
	}

	now := s.now()
	user.SyncAt = &now
	user.SyncStatus = "synced"
	user.UpdatedAt = now
	user.LastLoginAt = &now

	rolesFromAuth := extractRolesFromAuthServiceUser(authUserData, user.Email)
	permissionsFromAuth := extractPermissionsFromAuthServiceUser(authUserData)
	if len(rolesFromAuth) > 0 {
		user.Roles = mergeRoles(user.Roles, rolesFromAuth)
	}
	if len(permissionsFromAuth) > 0 {
		user.Permissions = mergePermissions(user.Permissions, permissionsFromAuth)
	} else if len(rolesFromAuth) > 0 {
		user.Permissions = ConsolidatePermissions(user.Roles)
	}

	if err := s.repo.UpdateUser(ctx, user); err != nil {
		return nil, fmt.Errorf("identity: update user from auth-service: %w", err)
	}

	return user, nil
}

func (s *Service) createUserFromAuthService(ctx context.Context, authServiceUserID uuid.UUID, tenantID string, authUserData map[string]interface{}) (*User, error) {
	email, _ := authUserData["email"].(string)
	fullName, _ := authUserData["full_name"].(string)
	if fullName == "" {
		if name, ok := authUserData["name"].(string); ok {
			fullName = name
		}
	}
	if fullName == "" {
		fullName = email
	}

	phone, _ := authUserData["phone"].(string)
	status, _ := authUserData["status"].(string)
	if status == "" {
		status = "active"
	}

	// Default role for notifications-service is viewer
	roles := []Role{RoleViewer}
	rolesFromAuth := extractRolesFromAuthServiceUser(authUserData, email)
	if len(rolesFromAuth) > 0 {
		roles = mergeRoles(roles, rolesFromAuth)
	}

	permissionsFromAuth := extractPermissionsFromAuthServiceUser(authUserData)
	var permissions []Permission
	if len(permissionsFromAuth) > 0 {
		permissions = permissionsFromAuth
	} else {
		permissions = ConsolidatePermissions(roles)
	}

	emailVerified, _ := authUserData["email_verified"].(bool)

	now := s.now()
	user := &User{
		ID:                authServiceUserID,
		TenantID:          tenantID,
		AuthServiceUserID: &authServiceUserID,
		Email:             email,
		EmailVerified:     emailVerified,
		FullName:          fullName,
		Phone:             phone,
		Status:            status,
		Roles:             roles,
		Permissions:       permissions,
		SyncStatus:        "synced",
		SyncAt:            &now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := s.repo.CreateUser(ctx, user); err != nil {
		return nil, fmt.Errorf("identity: create user from auth-service: %w", err)
	}

	return user, nil
}

// extractRolesFromAuthServiceUser extracts roles from auth-service user data.
func extractRolesFromAuthServiceUser(authUserData map[string]interface{}, email string) []Role {
	var roles []Role

	// Platform owner with superuser role gets full admin access
	isPlatformOwner, _ := authUserData["is_platform_owner"].(bool)
	if isPlatformOwner {
		return []Role{RoleSuperAdmin, RoleAdmin}
	}

	// Check for roles array ([]interface{} from JSON)
	if rolesData, ok := authUserData["roles"].([]interface{}); ok {
		for _, r := range rolesData {
			if roleStr, ok := r.(string); ok {
				switch roleStr {
				case "superuser":
					roles = append(roles, RoleSuperAdmin)
				case "admin":
					roles = append(roles, RoleAdmin)
				case "manager":
					roles = append(roles, RoleManager)
				case "viewer", "user", "customer":
					roles = append(roles, RoleViewer)
				}
			}
		}
	}

	// Also check for string roles
	if rolesData, ok := authUserData["roles"].([]string); ok {
		for _, roleStr := range rolesData {
			switch strings.ToLower(roleStr) {
			case "superuser":
				roles = append(roles, RoleSuperAdmin)
			case "admin":
				roles = append(roles, RoleAdmin)
			case "manager":
				roles = append(roles, RoleManager)
			case "viewer", "user", "customer":
				roles = append(roles, RoleViewer)
			}
		}
	}

	return roles
}

// extractPermissionsFromAuthServiceUser extracts permissions from auth-service user data.
func extractPermissionsFromAuthServiceUser(authUserData map[string]interface{}) []Permission {
	var permissions []Permission

	if permsData, ok := authUserData["permissions"].([]interface{}); ok {
		for _, p := range permsData {
			if permStr, ok := p.(string); ok {
				permissions = append(permissions, Permission(permStr))
			}
		}
	}

	if permsData, ok := authUserData["permissions"].([]string); ok {
		for _, permStr := range permsData {
			permissions = append(permissions, Permission(permStr))
		}
	}

	return permissions
}

// mergePermissions merges two permission slices, removing duplicates.
func mergePermissions(existing []Permission, newPerms []Permission) []Permission {
	permMap := make(map[Permission]bool)
	for _, p := range existing {
		permMap[p] = true
	}
	for _, p := range newPerms {
		permMap[p] = true
	}
	var merged []Permission
	for p := range permMap {
		merged = append(merged, p)
	}
	return merged
}

// mergeRoles merges two role slices, removing duplicates.
func mergeRoles(existing []Role, newRoles []Role) []Role {
	roleMap := make(map[Role]bool)
	for _, r := range existing {
		roleMap[r] = true
	}
	for _, r := range newRoles {
		roleMap[r] = true
	}
	var merged []Role
	for r := range roleMap {
		merged = append(merged, r)
	}
	return merged
}
