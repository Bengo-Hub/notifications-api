package identity

import (
	"time"

	"github.com/google/uuid"
)

// Role represents the high-level access tier for a user.
type Role string

const (
	RoleViewer     Role = "viewer"
	RoleManager    Role = "manager"
	RoleAdmin      Role = "admin"
	RoleSuperAdmin Role = "superuser"
)

// Permission captures fine-grained feature access across the notifications platform.
type Permission string

const (
	PermNotificationsRead   Permission = "notifications.notifications.view"
	PermNotificationsSend   Permission = "notifications.notifications.add"
	PermNotificationsManage Permission = "notifications.notifications.manage"
	PermTemplatesRead       Permission = "notifications.templates.view"
	PermTemplatesManage     Permission = "notifications.templates.manage"
	PermTemplatesTest       Permission = "notifications.templates.manage"
	PermProvidersRead       Permission = "notifications.providers.view"
	PermProvidersManage     Permission = "notifications.providers.manage"
	PermSettingsRead        Permission = "notifications.config.view"
	PermSettingsManage      Permission = "notifications.config.manage"
	PermBillingRead         Permission = "notifications.billing.view"
	PermBillingManage       Permission = "notifications.billing.manage"
	PermAnalyticsRead       Permission = "notifications.analytics.view"
	PermAnalyticsExport     Permission = "notifications.analytics.manage"
	PermCreditsRead         Permission = "notifications.credits.view"
	PermCreditsManage       Permission = "notifications.credits.manage"
	PermUsersRead           Permission = "notifications.users.view"
	PermUsersManage         Permission = "notifications.users.manage"
	PermPlatformProviders   Permission = "notifications.platform.providers"
	PermPlatformBilling     Permission = "notifications.platform.billing"
)

// AllPermissions returns all defined permissions.
func AllPermissions() []Permission {
	return []Permission{
		PermNotificationsRead, PermNotificationsSend, PermNotificationsManage,
		PermTemplatesRead, PermTemplatesManage, PermTemplatesTest,
		PermProvidersRead, PermProvidersManage,
		PermSettingsRead, PermSettingsManage,
		PermBillingRead, PermBillingManage,
		PermAnalyticsRead, PermAnalyticsExport,
		PermCreditsRead, PermCreditsManage,
		PermUsersRead, PermUsersManage,
		PermPlatformProviders, PermPlatformBilling,
	}
}

// DefaultPermissions returns the permissions granted to the supplied role.
func DefaultPermissions(role Role) []Permission {
	switch role {
	case RoleViewer:
		return []Permission{
			PermNotificationsRead,
			PermTemplatesRead,
			PermProvidersRead,
			PermSettingsRead,
			PermBillingRead,
			PermAnalyticsRead,
			PermCreditsRead,
		}
	case RoleManager:
		return []Permission{
			PermNotificationsRead, PermNotificationsSend, PermNotificationsManage,
			PermTemplatesRead, PermTemplatesManage, PermTemplatesTest,
			PermProvidersRead, PermProvidersManage,
			PermSettingsRead, PermSettingsManage,
			PermBillingRead,
			PermAnalyticsRead, PermAnalyticsExport,
			PermCreditsRead, PermCreditsManage,
		}
	case RoleAdmin:
		return []Permission{
			PermNotificationsRead, PermNotificationsSend, PermNotificationsManage,
			PermTemplatesRead, PermTemplatesManage, PermTemplatesTest,
			PermProvidersRead, PermProvidersManage,
			PermSettingsRead, PermSettingsManage,
			PermBillingRead, PermBillingManage,
			PermAnalyticsRead, PermAnalyticsExport,
			PermCreditsRead, PermCreditsManage,
			PermUsersRead, PermUsersManage,
		}
	case RoleSuperAdmin:
		return AllPermissions()
	default:
		return []Permission{}
	}
}

// ConsolidatePermissions calculates the union of permissions for a list of roles.
func ConsolidatePermissions(roles []Role) []Permission {
	permMap := make(map[Permission]bool)
	for _, r := range roles {
		for _, p := range DefaultPermissions(r) {
			permMap[p] = true
		}
	}
	var perms []Permission
	for p := range permMap {
		perms = append(perms, p)
	}
	if perms == nil {
		return []Permission{}
	}
	return perms
}

// User model stored in persistence.
type User struct {
	ID                uuid.UUID              `json:"id"`
	TenantID          string                 `json:"tenantId"`
	AuthServiceUserID *uuid.UUID             `json:"authServiceUserId,omitempty"`
	Email             string                 `json:"email"`
	EmailVerified     bool                   `json:"emailVerified"`
	FullName          string                 `json:"fullName"`
	Phone             string                 `json:"phone"`
	Roles             []Role                 `json:"roles"`
	Permissions       []Permission           `json:"permissions"`
	SyncStatus        string                 `json:"syncStatus,omitempty"`
	SyncAt            *time.Time             `json:"syncAt,omitempty"`
	LastLoginAt       *time.Time             `json:"lastLoginAt"`
	CreatedAt         time.Time              `json:"createdAt"`
	UpdatedAt         time.Time              `json:"updatedAt"`
	Status            string                 `json:"status"`
	Metadata          map[string]interface{} `json:"metadata"`
}

// HasRole checks whether the user has the provided role.
func (u *User) HasRole(role Role) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// HasPermission checks whether the user has the provided permission.
func (u *User) HasPermission(permission Permission) bool {
	for _, p := range u.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}
