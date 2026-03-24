package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/tenant"
	"github.com/bengobox/notifications-api/internal/ent/user"
)

// EntRepository implements the Repository interface using Ent as the persistence layer.
type EntRepository struct {
	client *ent.Client
}

// NewEntRepository constructs an Ent-backed repository.
func NewEntRepository(client *ent.Client) *EntRepository {
	return &EntRepository{client: client}
}

// CreateUser persists a new user record.
func (r *EntRepository) CreateUser(ctx context.Context, usr *User) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("identity: begin tx: %w", err)
	}

	if err := upsertRoles(ctx, tx, usr.Roles); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := upsertUser(ctx, tx.Client(), usr); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

// UpdateUser mutates persisted user information.
func (r *EntRepository) UpdateUser(ctx context.Context, usr *User) error {
	if usr == nil {
		return errors.New("identity: nil user update")
	}
	metadata := usr.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	builder := r.client.User.UpdateOneID(usr.ID).
		SetFullName(usr.FullName).
		SetStatus(usr.Status).
		SetMetadata(metadata).
		SetNillablePhone(optionalString(usr.Phone)).
		SetNillableLastLoginAt(usr.LastLoginAt).
		SetNillableAuthServiceUserID(usr.AuthServiceUserID).
		SetNillableSyncAt(usr.SyncAt)

	if usr.SyncStatus != "" {
		builder.SetSyncStatus(usr.SyncStatus)
	}

	_, err := builder.Save(ctx)
	if err != nil {
		return fmt.Errorf("identity: update user: %w", err)
	}

	if err := r.syncUserRoles(ctx, usr.ID, usr.Roles); err != nil {
		return err
	}
	return nil
}

// FindUserByEmail fetches a user by email (case-insensitive).
func (r *EntRepository) FindUserByEmail(ctx context.Context, email string) (*User, error) {
	u, err := r.client.User.
		Query().
		Where(user.EmailEqualFold(email)).
		WithTenant().
		WithRoles(func(q *ent.RoleQuery) {
			q.WithPermissions()
		}).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("identity: find user by email: %w", err)
	}
	return mapEntUser(u), nil
}

// FindUserByAuthServiceID fetches a user by auth-service user ID.
func (r *EntRepository) FindUserByAuthServiceID(ctx context.Context, authServiceUserID uuid.UUID) (*User, error) {
	u, err := r.client.User.
		Query().
		Where(user.AuthServiceUserIDEQ(authServiceUserID)).
		WithTenant().
		WithRoles(func(q *ent.RoleQuery) {
			q.WithPermissions()
		}).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("identity: find user by auth-service ID: %w", err)
	}
	return mapEntUser(u), nil
}

// FindUserByID fetches a user by identifier.
func (r *EntRepository) FindUserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	u, err := r.client.User.
		Query().
		Where(user.IDEQ(id)).
		WithTenant().
		WithRoles(func(q *ent.RoleQuery) {
			q.WithPermissions()
		}).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("identity: find user by id: %w", err)
	}
	return mapEntUser(u), nil
}

// ListUsers returns all users.
func (r *EntRepository) ListUsers(ctx context.Context) ([]*User, error) {
	records, err := r.client.User.
		Query().
		WithTenant().
		WithRoles(func(q *ent.RoleQuery) { q.WithPermissions() }).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: list users: %w", err)
	}
	out := make([]*User, 0, len(records))
	for _, u := range records {
		out = append(out, mapEntUser(u))
	}
	return out, nil
}

// FindTenantBySlug finds a tenant by its slug.
func (r *EntRepository) FindTenantBySlug(ctx context.Context, slug string) (*Tenant, error) {
	tenantEntity, err := r.client.Tenant.Query().
		Where(tenant.SlugEQ(slug)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("identity: tenant not found: %s", slug)
		}
		return nil, fmt.Errorf("identity: find tenant by slug: %w", err)
	}

	return &Tenant{
		ID:     tenantEntity.ID,
		Slug:   tenantEntity.Slug,
		Name:   tenantEntity.Name,
		Status: tenantEntity.Status,
	}, nil
}

// FindTenantByID finds a tenant by its ID.
func (r *EntRepository) FindTenantByID(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	tenantEntity, err := r.client.Tenant.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("identity: tenant not found: %s", id)
		}
		return nil, fmt.Errorf("identity: find tenant by id: %w", err)
	}

	return &Tenant{
		ID:     tenantEntity.ID,
		Slug:   tenantEntity.Slug,
		Name:   tenantEntity.Name,
		Status: tenantEntity.Status,
	}, nil
}

// UpsertTenant creates or updates a tenant.
func (r *EntRepository) UpsertTenant(ctx context.Context, t *Tenant) error {
	if t == nil {
		return errors.New("identity: nil tenant upsert")
	}

	err := r.client.Tenant.UpdateOneID(t.ID).
		SetSlug(t.Slug).
		SetName(t.Name).
		SetStatus(t.Status).
		Exec(ctx)

	if err != nil {
		if ent.IsNotFound(err) {
			err = r.client.Tenant.Create().
				SetID(t.ID).
				SetSlug(t.Slug).
				SetName(t.Name).
				SetStatus(t.Status).
				Exec(ctx)
			if err != nil {
				return fmt.Errorf("identity: create tenant: %w", err)
			}
		} else {
			return fmt.Errorf("identity: update tenant: %w", err)
		}
	}
	return nil
}

func (r *EntRepository) syncUserRoles(ctx context.Context, userID uuid.UUID, roles []Role) error {
	roleIDs := make([]string, 0, len(roles))
	for _, role := range roles {
		roleIDs = append(roleIDs, string(role))
	}
	return r.client.User.UpdateOneID(userID).ClearRoles().AddRoleIDs(roleIDs...).Exec(ctx)
}

func mapEntUser(u *ent.User) *User {
	var (
		roles       []Role
		permissions = make(map[Permission]struct{})
	)
	for _, r := range u.Edges.Roles {
		roles = append(roles, Role(r.ID))
		for _, perm := range r.Edges.Permissions {
			permissions[Permission(perm.Name)] = struct{}{}
		}
	}
	var perms []Permission
	for p := range permissions {
		perms = append(perms, p)
	}

	var authServiceUserID *uuid.UUID
	if u.AuthServiceUserID != uuid.Nil {
		authServiceUserID = &u.AuthServiceUserID
	}
	syncStatus := u.SyncStatus
	var syncAt *time.Time
	if !u.SyncAt.IsZero() {
		syncAt = &u.SyncAt
	}

	return &User{
		ID:                u.ID,
		TenantID:          u.TenantID.String(),
		AuthServiceUserID: authServiceUserID,
		Email:             u.Email,
		FullName:          u.FullName,
		Phone:             u.Phone,
		Roles:             roles,
		Permissions:       perms,
		SyncStatus:        syncStatus,
		SyncAt:            syncAt,
		LastLoginAt:       optionalTime(u.LastLoginAt),
		CreatedAt:         u.CreatedAt,
		UpdatedAt:         u.UpdatedAt,
		Status:            u.Status,
		Metadata:          u.Metadata,
	}
}

func upsertRoles(ctx context.Context, tx *ent.Tx, roles []Role) error {
	for _, role := range roles {
		_, err := tx.Role.UpdateOneID(string(role)).
			SetName(string(role)).
			SetDescription("").
			Save(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				if err := tx.Role.
					Create().
					SetID(string(role)).
					SetName(string(role)).
					SetDescription("").
					Exec(ctx); err != nil {
					return fmt.Errorf("identity: create role %s: %w", role, err)
				}
			} else {
				return fmt.Errorf("identity: upsert role %s: %w", role, err)
			}
		}
	}
	return nil
}

func upsertUser(ctx context.Context, client *ent.Client, usr *User) error {
	if usr == nil {
		return errors.New("identity: nil user upsert")
	}
	if usr.ID == uuid.Nil {
		usr.ID = uuid.New()
	}

	tenantUUID, err := uuid.Parse(usr.TenantID)
	if err != nil {
		return fmt.Errorf("identity: invalid tenant id %q: %w", usr.TenantID, err)
	}

	metadata := usr.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	builder := client.User.
		Create().
		SetID(usr.ID).
		SetTenantID(tenantUUID).
		SetEmail(usr.Email).
		SetFullName(usr.FullName).
		SetNillablePhone(optionalString(usr.Phone)).
		SetStatus(usr.Status).
		SetLocale("en").
		SetMetadata(metadata).
		SetCreatedAt(usr.CreatedAt).
		SetUpdatedAt(usr.UpdatedAt).
		SetNillableLastLoginAt(usr.LastLoginAt)

	if usr.AuthServiceUserID != nil {
		builder.SetAuthServiceUserID(*usr.AuthServiceUserID)
	}
	if usr.SyncStatus != "" {
		builder.SetSyncStatus(usr.SyncStatus)
	}
	if usr.SyncAt != nil {
		builder.SetSyncAt(*usr.SyncAt)
	}

	roleIDs := make([]string, 0, len(usr.Roles))
	for _, role := range usr.Roles {
		roleIDs = append(roleIDs, string(role))
	}
	builder.AddRoleIDs(roleIDs...)

	// Upsert: try update first, then create if not exists
	_, err = client.User.UpdateOneID(usr.ID).
		SetTenantID(tenantUUID).
		SetEmail(usr.Email).
		SetFullName(usr.FullName).
		SetNillablePhone(optionalString(usr.Phone)).
		SetStatus(usr.Status).
		SetMetadata(metadata).
		SetUpdatedAt(usr.UpdatedAt).
		SetNillableLastLoginAt(usr.LastLoginAt).
		Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			if err := builder.Exec(ctx); err != nil {
				// Handle duplicate key (race condition)
				if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "UNIQUE constraint") {
					return nil
				}
				return fmt.Errorf("identity: create user: %w", err)
			}
		} else {
			return fmt.Errorf("identity: update user: %w", err)
		}
	} else {
		// Update auth-service fields and roles
		updateBuilder := client.User.UpdateOneID(usr.ID)
		if usr.AuthServiceUserID != nil {
			updateBuilder.SetAuthServiceUserID(*usr.AuthServiceUserID)
		}
		if usr.SyncStatus != "" {
			updateBuilder.SetSyncStatus(usr.SyncStatus)
		}
		if usr.SyncAt != nil {
			updateBuilder.SetSyncAt(*usr.SyncAt)
		}
		if _, err := updateBuilder.Save(ctx); err != nil {
			return fmt.Errorf("identity: update user auth fields: %w", err)
		}
	}

	repo := NewEntRepository(client)
	return repo.syncUserRoles(ctx, usr.ID, usr.Roles)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
