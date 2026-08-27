package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/rbac"
)

// RBACHandler handles RBAC-related operations.
type RBACHandler struct {
	logger      *zap.Logger
	rbacService *rbac.Service
	rbacRepo    rbac.Repository
}

// NewRBACHandler creates a new RBAC handler.
func NewRBACHandler(logger *zap.Logger, rbacService *rbac.Service, rbacRepo rbac.Repository) *RBACHandler {
	return &RBACHandler{
		logger:      logger,
		rbacService: rbacService,
		rbacRepo:    rbacRepo,
	}
}

// AssignRoleRequest represents a request to assign a role.
type AssignRoleRequest struct {
	UserID uuid.UUID `json:"user_id"`
	RoleID uuid.UUID `json:"role_id"`
}

// AssignRole assigns a role to a user.
func (h *RBACHandler) AssignRole(w http.ResponseWriter, r *http.Request) {
	tenantID, err := resolveTenantID(r)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid tenant ID")
		return
	}

	var req AssignRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	assignedBy, err := claims.UserID()
	if err != nil || assignedBy == uuid.Nil {
		RespondError(w, http.StatusUnauthorized, "invalid user ID")
		return
	}

	if err := h.rbacService.AssignRole(r.Context(), tenantID, req.UserID, req.RoleID, assignedBy); err != nil {
		h.logger.Error("failed to assign role", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to assign role")
		return
	}

	respondJSON(w, http.StatusCreated, map[string]string{"message": "role assigned successfully"})
}

// RevokeRole revokes a role from a user.
func (h *RBACHandler) RevokeRole(w http.ResponseWriter, r *http.Request) {
	tenantID, err := resolveTenantID(r)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid tenant ID")
		return
	}

	assignmentIDStr := chi.URLParam(r, "id")
	assignmentID, err := uuid.Parse(assignmentIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid assignment ID")
		return
	}

	// Get assignment to extract user ID and role ID
	assignments, err := h.rbacRepo.ListUserAssignments(r.Context(), tenantID, rbac.AssignmentFilters{})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "failed to get assignment")
		return
	}

	var assignment *rbac.UserRoleAssignment
	for _, a := range assignments {
		if a.ID == assignmentID {
			assignment = a
			break
		}
	}

	if assignment == nil {
		RespondError(w, http.StatusNotFound, "assignment not found")
		return
	}

	if err := h.rbacService.RevokeRole(r.Context(), tenantID, assignment.UserID, assignment.RoleID); err != nil {
		h.logger.Error("failed to revoke role", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to revoke role")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"message": "role revoked successfully"})
}

// ListAssignments lists all role assignments.
func (h *RBACHandler) ListAssignments(w http.ResponseWriter, r *http.Request) {
	tenantID, err := resolveTenantID(r)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid tenant ID")
		return
	}

	assignments, err := h.rbacRepo.ListUserAssignments(r.Context(), tenantID, rbac.AssignmentFilters{})
	if err != nil {
		h.logger.Error("failed to list assignments", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to list assignments")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"assignments": assignments})
}

// ListRoles lists all roles.
func (h *RBACHandler) ListRoles(w http.ResponseWriter, r *http.Request) {
	tenantID, err := resolveTenantID(r)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid tenant ID")
		return
	}

	roles, err := h.rbacRepo.ListRoles(r.Context(), tenantID)
	if err != nil {
		h.logger.Error("failed to list roles", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to list roles")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"roles": roles})
}

// ListPermissions lists all permissions.
func (h *RBACHandler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	module := r.URL.Query().Get("module")
	action := r.URL.Query().Get("action")

	filters := rbac.PermissionFilters{}
	if module != "" {
		filters.Module = &module
	}
	if action != "" {
		filters.Action = &action
	}

	permissions, err := h.rbacRepo.ListPermissions(r.Context(), filters)
	if err != nil {
		h.logger.Error("failed to list permissions", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to list permissions")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"permissions": permissions})
}

// GetUserRoles returns roles for a specific user.
func (h *RBACHandler) GetUserRoles(w http.ResponseWriter, r *http.Request) {
	tenantID, err := resolveTenantID(r)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid tenant ID")
		return
	}

	userIDStr := chi.URLParam(r, "userId")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid user ID")
		return
	}

	roles, err := h.rbacService.GetUserRoles(r.Context(), tenantID, userID)
	if err != nil {
		h.logger.Error("failed to get user roles", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to get user roles")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"roles": roles})
}

// GetUserPermissions returns permissions for a specific user.
func (h *RBACHandler) GetUserPermissions(w http.ResponseWriter, r *http.Request) {
	tenantID, err := resolveTenantID(r)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid tenant ID")
		return
	}

	userIDStr := chi.URLParam(r, "userId")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid user ID")
		return
	}

	permissions, err := h.rbacService.GetUserPermissions(r.Context(), tenantID, userID)
	if err != nil {
		h.logger.Error("failed to get user permissions", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to get user permissions")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"permissions": permissions})
}

// RegisterRoutes registers RBAC routes on the provided router.
func (h *RBACHandler) RegisterRoutes(r chi.Router) {
	r.Route("/rbac", func(rbacRouter chi.Router) {
		rbacRouter.Get("/roles", h.ListRoles)
		rbacRouter.Get("/permissions", h.ListPermissions)
		rbacRouter.Get("/assignments", h.ListAssignments)
		rbacRouter.Post("/assignments", h.AssignRole)
		rbacRouter.Delete("/assignments/{id}", h.RevokeRole)
		rbacRouter.Get("/users/{userId}/roles", h.GetUserRoles)
		rbacRouter.Get("/users/{userId}/permissions", h.GetUserPermissions)
	})
}

// resolveTenantID extracts the tenant ID this RBAC request should act on — honoring the
// platform-admin tenant-switcher (X-Tenant-ID header, then legacy ?tenantId=) via
// resolveActingTenantID, falling back to a URL param if present.
func resolveTenantID(r *http.Request) (uuid.UUID, error) {
	tenantIDStr := resolveActingTenantID(r)
	if tenantIDStr == "" {
		// Try URL param as fallback
		tenantIDStr = chi.URLParam(r, "tenant")
	}
	return uuid.Parse(tenantIDStr)
}
