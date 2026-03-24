package identity

import (
	"context"

	"github.com/google/uuid"
)

// Tenant represents a tenant entity from the database.
type Tenant struct {
	ID     uuid.UUID
	Slug   string
	Name   string
	Status string
}

// Repository abstracts persistence for identity entities.
type Repository interface {
	CreateUser(ctx context.Context, user *User) error
	UpdateUser(ctx context.Context, user *User) error
	FindUserByEmail(ctx context.Context, email string) (*User, error)
	FindUserByID(ctx context.Context, id uuid.UUID) (*User, error)
	FindUserByAuthServiceID(ctx context.Context, authServiceUserID uuid.UUID) (*User, error)
	ListUsers(ctx context.Context) ([]*User, error)

	FindTenantBySlug(ctx context.Context, slug string) (*Tenant, error)
	FindTenantByID(ctx context.Context, id uuid.UUID) (*Tenant, error)
	UpsertTenant(ctx context.Context, tenant *Tenant) error
}
