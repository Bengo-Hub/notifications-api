package rbac

import (
	"time"

	"github.com/google/uuid"
)

// NotificationRole represents a notification service role.
type NotificationRole struct {
	ID           uuid.UUID
	TenantID     *uuid.UUID // nil = global/system role shared platform-wide
	RoleCode     string
	Name         string
	Description  *string
	IsSystemRole bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NotificationPermission represents a notification service permission.
type NotificationPermission struct {
	ID             uuid.UUID
	PermissionCode string
	Name           string
	Module         string
	Action         string
	Resource       *string
	Description    *string
	CreatedAt      time.Time
}

// UserRoleAssignment represents a user role assignment.
type UserRoleAssignment struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	UserID     uuid.UUID
	RoleID     uuid.UUID
	AssignedBy uuid.UUID
	AssignedAt time.Time
	ExpiresAt  *time.Time
}
