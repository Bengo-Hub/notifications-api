package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/notificationpermission"
	"github.com/bengobox/notifications-api/internal/ent/notificationrole"
	"github.com/bengobox/notifications-api/internal/ent/notificationrolepermission"
	"github.com/bengobox/notifications-api/internal/ent/userroleassignment"
	"github.com/google/uuid"
)

// EntRepository implements the Repository interface using Ent ORM.
type EntRepository struct {
	client *ent.Client
}

// NewEntRepository creates a new Ent-backed repository.
func NewEntRepository(client *ent.Client) *EntRepository {
	return &EntRepository{client: client}
}

// CreateRole persists a new role.
func (r *EntRepository) CreateRole(ctx context.Context, tenantID uuid.UUID, role *NotificationRole) error {
	if role == nil {
		return errors.New("role cannot be nil")
	}

	builder := r.client.NotificationRole.Create().
		SetID(role.ID).
		SetTenantID(tenantID).
		SetRoleCode(role.RoleCode).
		SetName(role.Name).
		SetIsSystemRole(role.IsSystemRole)

	if role.Description != nil {
		builder.SetDescription(*role.Description)
	}

	_, err := builder.Save(ctx)
	if err != nil {
		return fmt.Errorf("create role: %w", err)
	}

	return nil
}

// GetRole retrieves a role by ID. Roles are platform-wide: visible to a tenant if global (tenant_id
// NULL) or owned by that tenant (custom role).
func (r *EntRepository) GetRole(ctx context.Context, tenantID uuid.UUID, roleID uuid.UUID) (*NotificationRole, error) {
	entRole, err := r.client.NotificationRole.Query().
		Where(
			notificationrole.ID(roleID),
			notificationrole.Or(
				notificationrole.TenantID(tenantID),
				notificationrole.TenantIDIsNil(),
			),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("role not found: %w", err)
		}
		return nil, fmt.Errorf("get role: %w", err)
	}

	return mapEntRole(entRole), nil
}

// GetRoleByCode retrieves a role by code, preferring a tenant-specific custom role over the shared
// global/system role of the same code (hybrid: global system roles + optional per-tenant overrides).
func (r *EntRepository) GetRoleByCode(ctx context.Context, tenantID uuid.UUID, roleCode string) (*NotificationRole, error) {
	entRoles, err := r.client.NotificationRole.Query().
		Where(
			notificationrole.RoleCode(roleCode),
			notificationrole.Or(
				notificationrole.TenantID(tenantID),
				notificationrole.TenantIDIsNil(),
			),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("get role by code: %w", err)
	}
	if len(entRoles) == 0 {
		return nil, fmt.Errorf("role not found: %s", roleCode)
	}

	return mapEntRole(preferTenantRole(entRoles)), nil
}

// ListRoles lists the roles visible to a tenant: global/system roles (shared) plus the tenant's own
// custom roles, preferring a tenant override over the global role of the same code.
func (r *EntRepository) ListRoles(ctx context.Context, tenantID uuid.UUID) ([]*NotificationRole, error) {
	entRoles, err := r.client.NotificationRole.Query().
		Where(notificationrole.Or(
			notificationrole.TenantID(tenantID),
			notificationrole.TenantIDIsNil(),
		)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}

	byCode := make(map[string]*ent.NotificationRole, len(entRoles))
	for _, er := range entRoles {
		existing, ok := byCode[er.RoleCode]
		if !ok || (existing.TenantID == nil && er.TenantID != nil) {
			byCode[er.RoleCode] = er
		}
	}

	roles := make([]*NotificationRole, 0, len(byCode))
	for _, er := range byCode {
		roles = append(roles, mapEntRole(er))
	}

	return roles, nil
}

// preferTenantRole returns the tenant-specific role if present, otherwise the first (global) role.
func preferTenantRole(roles []*ent.NotificationRole) *ent.NotificationRole {
	chosen := roles[0]
	for _, er := range roles {
		if er.TenantID != nil {
			return er
		}
	}
	return chosen
}

// CreatePermission persists a new permission.
func (r *EntRepository) CreatePermission(ctx context.Context, permission *NotificationPermission) error {
	if permission == nil {
		return errors.New("permission cannot be nil")
	}

	builder := r.client.NotificationPermission.Create().
		SetID(permission.ID).
		SetPermissionCode(permission.PermissionCode).
		SetName(permission.Name).
		SetModule(permission.Module).
		SetAction(permission.Action)

	if permission.Resource != nil {
		builder.SetResource(*permission.Resource)
	}
	if permission.Description != nil {
		builder.SetDescription(*permission.Description)
	}

	_, err := builder.Save(ctx)
	if err != nil {
		return fmt.Errorf("create permission: %w", err)
	}

	return nil
}

// GetPermission retrieves a permission by ID.
func (r *EntRepository) GetPermission(ctx context.Context, permissionID uuid.UUID) (*NotificationPermission, error) {
	entPerm, err := r.client.NotificationPermission.Get(ctx, permissionID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("permission not found: %w", err)
		}
		return nil, fmt.Errorf("get permission: %w", err)
	}

	return mapEntPermission(entPerm), nil
}

// GetPermissionByCode retrieves a permission by code.
func (r *EntRepository) GetPermissionByCode(ctx context.Context, permissionCode string) (*NotificationPermission, error) {
	entPerm, err := r.client.NotificationPermission.Query().
		Where(notificationpermission.PermissionCode(permissionCode)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("permission not found: %w", err)
		}
		return nil, fmt.Errorf("get permission by code: %w", err)
	}

	return mapEntPermission(entPerm), nil
}

// ListPermissions lists permissions with optional filters.
func (r *EntRepository) ListPermissions(ctx context.Context, filters PermissionFilters) ([]*NotificationPermission, error) {
	query := r.client.NotificationPermission.Query()

	if filters.Module != nil {
		query = query.Where(notificationpermission.Module(*filters.Module))
	}
	if filters.Action != nil {
		query = query.Where(notificationpermission.Action(*filters.Action))
	}

	entPerms, err := query.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list permissions: %w", err)
	}

	permissions := make([]*NotificationPermission, len(entPerms))
	for i, entPerm := range entPerms {
		permissions[i] = mapEntPermission(entPerm)
	}

	return permissions, nil
}

// AssignPermissionToRole assigns a permission to a role.
func (r *EntRepository) AssignPermissionToRole(ctx context.Context, roleID uuid.UUID, permissionID uuid.UUID) error {
	_, err := r.client.NotificationRolePermission.Create().
		SetRoleID(roleID).
		SetPermissionID(permissionID).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("assign permission to role: %w", err)
	}

	return nil
}

// RemovePermissionFromRole removes a permission from a role.
func (r *EntRepository) RemovePermissionFromRole(ctx context.Context, roleID uuid.UUID, permissionID uuid.UUID) error {
	_, err := r.client.NotificationRolePermission.Delete().
		Where(
			notificationrolepermission.RoleID(roleID),
			notificationrolepermission.PermissionID(permissionID),
		).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("remove permission from role: %w", err)
	}

	return nil
}

// GetRolePermissions retrieves all permissions for a role.
func (r *EntRepository) GetRolePermissions(ctx context.Context, roleID uuid.UUID) ([]*NotificationPermission, error) {
	entPerms, err := r.client.NotificationRole.Query().
		Where(notificationrole.ID(roleID)).
		QueryPermissions().
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("get role permissions: %w", err)
	}

	permissions := make([]*NotificationPermission, len(entPerms))
	for i, entPerm := range entPerms {
		permissions[i] = mapEntPermission(entPerm)
	}

	return permissions, nil
}

// AssignRoleToUser assigns a role to a user.
func (r *EntRepository) AssignRoleToUser(ctx context.Context, tenantID uuid.UUID, assignment *UserRoleAssignment) error {
	if assignment == nil {
		return errors.New("assignment cannot be nil")
	}

	builder := r.client.UserRoleAssignment.Create().
		SetID(assignment.ID).
		SetTenantID(tenantID).
		SetUserID(assignment.UserID).
		SetRoleID(assignment.RoleID).
		SetAssignedBy(assignment.AssignedBy)

	if assignment.ExpiresAt != nil {
		builder.SetExpiresAt(*assignment.ExpiresAt)
	}

	_, err := builder.Save(ctx)
	if err != nil {
		return fmt.Errorf("assign role to user: %w", err)
	}

	return nil
}

// RevokeRoleFromUser revokes a role from a user.
func (r *EntRepository) RevokeRoleFromUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, roleID uuid.UUID) error {
	_, err := r.client.UserRoleAssignment.Delete().
		Where(
			userroleassignment.TenantID(tenantID),
			userroleassignment.UserID(userID),
			userroleassignment.RoleID(roleID),
		).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("revoke role from user: %w", err)
	}

	return nil
}

// GetUserRoles retrieves all roles assigned to a user.
func (r *EntRepository) GetUserRoles(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*NotificationRole, error) {
	entRoles, err := r.client.UserRoleAssignment.Query().
		Where(
			userroleassignment.TenantID(tenantID),
			userroleassignment.UserID(userID),
		).
		QueryRole().
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user roles: %w", err)
	}

	roles := make([]*NotificationRole, len(entRoles))
	for i, entRole := range entRoles {
		roles[i] = mapEntRole(entRole)
	}

	return roles, nil
}

// GetUserPermissions retrieves all permissions for a user (via their roles).
func (r *EntRepository) GetUserPermissions(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*NotificationPermission, error) {
	// Get user's roles first
	roles, err := r.GetUserRoles(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}

	// Collect all unique permissions from all roles
	permissionMap := make(map[uuid.UUID]*NotificationPermission)
	for _, role := range roles {
		rolePerms, err := r.GetRolePermissions(ctx, role.ID)
		if err != nil {
			continue
		}
		for _, perm := range rolePerms {
			permissionMap[perm.ID] = perm
		}
	}

	permissions := make([]*NotificationPermission, 0, len(permissionMap))
	for _, perm := range permissionMap {
		permissions = append(permissions, perm)
	}

	return permissions, nil
}

// ListUserAssignments lists role assignments with optional filters.
func (r *EntRepository) ListUserAssignments(ctx context.Context, tenantID uuid.UUID, filters AssignmentFilters) ([]*UserRoleAssignment, error) {
	query := r.client.UserRoleAssignment.Query().
		Where(userroleassignment.TenantID(tenantID))

	if filters.UserID != nil {
		query = query.Where(userroleassignment.UserID(*filters.UserID))
	}
	if filters.RoleID != nil {
		query = query.Where(userroleassignment.RoleID(*filters.RoleID))
	}

	entAssignments, err := query.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list user assignments: %w", err)
	}

	assignments := make([]*UserRoleAssignment, len(entAssignments))
	for i, entAssignment := range entAssignments {
		assignments[i] = mapEntAssignment(entAssignment)
	}

	return assignments, nil
}

// Mapping functions

func mapEntRole(entRole *ent.NotificationRole) *NotificationRole {
	role := &NotificationRole{
		ID:           entRole.ID,
		TenantID:     entRole.TenantID,
		RoleCode:     entRole.RoleCode,
		Name:         entRole.Name,
		IsSystemRole: entRole.IsSystemRole,
		CreatedAt:    entRole.CreatedAt,
		UpdatedAt:    entRole.UpdatedAt,
	}

	if entRole.Description != "" {
		role.Description = &entRole.Description
	}

	return role
}

func mapEntPermission(entPerm *ent.NotificationPermission) *NotificationPermission {
	perm := &NotificationPermission{
		ID:             entPerm.ID,
		PermissionCode: entPerm.PermissionCode,
		Name:           entPerm.Name,
		Module:         entPerm.Module,
		Action:         entPerm.Action,
		CreatedAt:      entPerm.CreatedAt,
	}

	if entPerm.Resource != "" {
		perm.Resource = &entPerm.Resource
	}
	if entPerm.Description != "" {
		perm.Description = &entPerm.Description
	}

	return perm
}

func mapEntAssignment(entAssignment *ent.UserRoleAssignment) *UserRoleAssignment {
	assignment := &UserRoleAssignment{
		ID:         entAssignment.ID,
		TenantID:   entAssignment.TenantID,
		UserID:     entAssignment.UserID,
		RoleID:     entAssignment.RoleID,
		AssignedBy: entAssignment.AssignedBy,
		AssignedAt: entAssignment.AssignedAt,
	}

	if !entAssignment.ExpiresAt.IsZero() {
		assignment.ExpiresAt = &entAssignment.ExpiresAt
	}

	return assignment
}
