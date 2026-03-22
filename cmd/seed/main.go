package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/database"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/notificationpermission"
	"github.com/bengobox/notifications-api/internal/ent/notificationrole"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	"github.com/bengobox/notifications-api/internal/ent/ratelimitconfig"
	"github.com/bengobox/notifications-api/internal/ent/serviceconfig"
	"github.com/bengobox/notifications-api/internal/modules/identity"
	tenantmodule "github.com/bengobox/notifications-api/internal/modules/tenant"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

// platformTenantID is the well-known ID for platform-level (shared) data.
// All platform-managed provider configs use this as tenant_id.
const platformTenantID = "platform"

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx := context.Background()
	client, err := database.NewClient(ctx, cfg.Postgres)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer client.Close()

	if err := database.RunMigrations(ctx, client); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	// ── Phase 1: Platform-level shared providers ────────────────────────
	// These are global provider configurations available to all tenants.
	// Tenants select which provider to use via their own _preferred rows.
	seedPlatformProviders(ctx, client)

	// ── Phase 2: Tenant sync (optional, for dev/staging) ────────────────
	// In production, tenants are synced JIT via the TenantV2 middleware.
	// For dev/staging, sync a list of known slugs from SEED_TENANTS env var.
	tenantSlugs := os.Getenv("SEED_TENANTS")
	if tenantSlugs == "" {
		tenantSlugs = "codevertex,mss,urban-loft,kura,ultichange"
	}

	tenantSyncer := tenantmodule.NewSyncer(client, cfg.Services.AuthAPI)
	for _, slug := range strings.Split(tenantSlugs, ",") {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}

		id, err := tenantSyncer.SyncTenant(ctx, slug)
		if err != nil {
			fmt.Printf("  ! tenant sync failed: %s (%v)\n", slug, err)
			continue
		}
		fmt.Printf("  synced tenant: %s (%s)\n", slug, id)

		seedTenantDefaults(ctx, client, id.String(), slug)

		// Seed RBAC roles for each tenant
		seedNotificationRoles(ctx, client, id)
	}

	// ── Phase 3: Identity — roles, permissions, role-permission mappings ─
	// (Legacy identity system — kept for backward compatibility)
	seedPermissions(ctx, client)
	seedRoles(ctx, client)
	seedRolePermissions(ctx, client)

	// ── Phase 4: New RBAC — notification permissions & role-permission mappings ─
	seedNotificationPermissions(ctx, client)
	seedNotificationRolePermissions(ctx, client)

	// ── Phase 5: Rate limit configs ─────────────────────────────────────
	seedRateLimitConfigs(ctx, client)

	// ── Phase 6: Service configs ────────────────────────────────────────
	seedServiceConfigs(ctx, client)

	// Optionally seed platform admin user
	if adminUserID := os.Getenv("SEED_PLATFORM_ADMIN_USER_ID"); adminUserID != "" {
		seedPlatformAdmin(ctx, client, tenantSyncer, adminUserID)
	}

	fmt.Println("seed complete")
}

// seedPlatformProviders creates platform-level provider config rows.
// These are shared across all tenants and managed by platform admins.
func seedPlatformProviders(ctx context.Context, client *ent.Client) {
	providers := []struct {
		providerType string
		providerName string
		envKey       string // if set, check env to determine active status
	}{
		{"email", "smtp", ""},
		{"email", "sendgrid", "SENDGRID_API_KEY"},
		{"email", "brevo", "BREVO_API_KEY"},
		{"sms", "twilio", "TWILIO_ACCOUNT_SID"},
		{"sms", "africastalking", "AFRICAS_TALKING_KEY"},
		{"sms", "vonage", "VONAGE_API_KEY"},
		{"sms", "plivo", "PLIVO_AUTH_ID"},
		{"push", "fcm", "FCM_SERVICE_ACCOUNT"},
		{"whatsapp", "apiwap", "APIWAP_API_KEY"},
	}

	for _, p := range providers {
		existing, _ := client.ProviderSetting.Query().
			Where(
				providersetting.TenantIDEQ(platformTenantID),
				providersetting.IsPlatform(true),
				providersetting.ProviderTypeEQ(p.providerType),
				providersetting.ProviderNameEQ(p.providerName),
				providersetting.KeyEQ("_config"),
			).First(ctx)

		isActive := true
		if p.envKey != "" && os.Getenv(p.envKey) == "" {
			isActive = false
		}

		if existing == nil {
			_, err := client.ProviderSetting.Create().
				SetTenantID(platformTenantID).
				SetChannel(p.providerType).
				SetProvider(p.providerName).
				SetProviderType(p.providerType).
				SetProviderName(p.providerName).
				SetKey("_config").
				SetValue("configured").
				SetIsPlatform(true).
				SetIsPlatformManaged(true).
				SetIsActive(isActive).
				SetStatus("active").
				Save(ctx)
			if err == nil {
				fmt.Printf("  platform provider: %s/%s (active=%v)\n", p.providerType, p.providerName, isActive)
			}
		} else {
			_, _ = existing.Update().SetIsActive(isActive).Save(ctx)
		}
	}
}

// seedTenantDefaults sets default preferred providers for a tenant.
// Only creates rows that don't already exist (idempotent).
func seedTenantDefaults(ctx context.Context, client *ent.Client, tenantID, slug string) {
	defaults := []struct {
		providerType string
		providerName string
	}{
		{"email", "smtp"},
		{"sms", "twilio"},
	}

	for _, d := range defaults {
		existing, _ := client.ProviderSetting.Query().
			Where(
				providersetting.TenantID(tenantID),
				providersetting.ProviderTypeEQ(d.providerType),
				providersetting.KeyEQ("_preferred"),
			).First(ctx)

		if existing != nil {
			continue
		}

		_, err := client.ProviderSetting.Create().
			SetTenantID(tenantID).
			SetChannel(d.providerType).
			SetProvider(d.providerName).
			SetProviderType(d.providerType).
			SetProviderName(d.providerName).
			SetKey("_preferred").
			SetValue(d.providerName).
			SetIsPlatform(false).
			SetIsActive(true).
			SetStatus("active").
			Save(ctx)
		if err == nil {
			fmt.Printf("    default %s for %s: %s\n", d.providerType, slug, d.providerName)
		}
	}
}

// permissionUUID generates a deterministic UUID from a permission code.
func permissionUUID(code string) uuid.UUID {
	hash := sha256.Sum256([]byte("notifications:" + code))
	return uuid.UUID(hash[:16])
}

// seedPermissions creates all notification-service permissions (legacy identity system).
func seedPermissions(ctx context.Context, client *ent.Client) {
	fmt.Println("  seeding permissions (legacy)...")

	type permDef struct {
		code        string
		module      string
		description string
	}

	perms := []permDef{
		{"notifications:read", "notifications", "View delivery logs and notification history"},
		{"notifications:send", "notifications", "Send/enqueue notifications"},
		{"notifications:manage", "notifications", "Full notification management"},
		{"templates:read", "templates", "View notification templates"},
		{"templates:manage", "templates", "Create, update, delete templates"},
		{"templates:test", "templates", "Send test notifications from templates"},
		{"providers:read", "providers", "View provider configurations"},
		{"providers:manage", "providers", "Configure notification providers"},
		{"settings:read", "settings", "View tenant settings"},
		{"settings:manage", "settings", "Update tenant settings"},
		{"billing:read", "billing", "View balance and transaction history"},
		{"billing:manage", "billing", "Initiate top-ups and manage billing"},
		{"analytics:read", "analytics", "View delivery analytics"},
		{"analytics:export", "analytics", "Export analytics data"},
		{"credits:read", "credits", "View credit balance"},
		{"credits:manage", "credits", "Manage credit transactions"},
		{"users:read", "users", "View users in tenant"},
		{"users:manage", "users", "Manage user roles"},
		{"platform:providers", "platform", "Manage platform-level providers"},
		{"platform:billing", "platform", "Manage platform billing settings"},
	}

	for _, p := range perms {
		id := permissionUUID(p.code)
		existing, _ := client.Permission.Get(ctx, id)
		if existing != nil {
			// Update in case description changed
			_, _ = existing.Update().
				SetName(p.code).
				SetModule(p.module).
				SetDescription(p.description).
				Save(ctx)
			continue
		}

		err := client.Permission.Create().
			SetID(id).
			SetName(p.code).
			SetModule(p.module).
			SetDescription(p.description).
			Exec(ctx)
		if err == nil {
			fmt.Printf("    permission: %s\n", p.code)
		} else {
			fmt.Printf("    ! permission %s: %v\n", p.code, err)
		}
	}
}

// seedRoles creates the system roles (legacy identity system).
func seedRoles(ctx context.Context, client *ent.Client) {
	fmt.Println("  seeding roles (legacy)...")

	type roleDef struct {
		id          string
		name        string
		scope       string
		description string
	}

	roles := []roleDef{
		{"viewer", "Viewer", "tenant", "Read-only access to notifications data"},
		{"manager", "Manager", "tenant", "Manage notifications, templates, providers"},
		{"admin", "Admin", "tenant", "Full tenant administration"},
		{"superuser", "Superuser", "global", "Platform-wide superuser access"},
	}

	for _, r := range roles {
		existing, _ := client.Role.Get(ctx, r.id)
		if existing != nil {
			_, _ = existing.Update().
				SetName(r.name).
				SetScope(r.scope).
				SetDescription(r.description).
				SetSystemRole(true).
				Save(ctx)
			fmt.Printf("    role (updated): %s (%s)\n", r.id, r.scope)
			continue
		}

		err := client.Role.Create().
			SetID(r.id).
			SetName(r.name).
			SetScope(r.scope).
			SetDescription(r.description).
			SetSystemRole(true).
			Exec(ctx)
		if err == nil {
			fmt.Printf("    role: %s (%s)\n", r.id, r.scope)
		} else {
			fmt.Printf("    ! role %s: %v\n", r.id, err)
		}
	}
}

// seedRolePermissions attaches permissions to roles per DefaultPermissions mapping (legacy).
func seedRolePermissions(ctx context.Context, client *ent.Client) {
	fmt.Println("  seeding role-permission mappings (legacy)...")

	roles := []identity.Role{
		identity.RoleViewer,
		identity.RoleManager,
		identity.RoleAdmin,
		identity.RoleSuperAdmin,
	}

	for _, role := range roles {
		perms := identity.DefaultPermissions(role)
		permIDs := make([]uuid.UUID, 0, len(perms))
		for _, p := range perms {
			permIDs = append(permIDs, permissionUUID(string(p)))
		}

		err := client.Role.UpdateOneID(string(role)).
			ClearPermissions().
			AddPermissionIDs(permIDs...).
			Exec(ctx)
		if err != nil {
			fmt.Printf("    ! role-perms %s: %v\n", role, err)
		} else {
			fmt.Printf("    role-perms: %s (%d permissions)\n", role, len(permIDs))
		}
	}
}

// notifPermUUID generates a deterministic UUID for notification RBAC permissions.
func notifPermUUID(code string) uuid.UUID {
	hash := sha256.Sum256([]byte("notif_rbac:" + code))
	return uuid.UUID(hash[:16])
}

// seedNotificationPermissions creates the new RBAC notification permissions.
func seedNotificationPermissions(ctx context.Context, client *ent.Client) {
	fmt.Println("  seeding notification permissions (RBAC)...")

	type permDef struct {
		code        string
		name        string
		module      string
		action      string
		resource    string
		description string
	}

	perms := []permDef{
		// Notifications
		{"notifications.notifications.add", "Send Notifications", "notifications", "add", "notifications", "Send/enqueue notifications"},
		{"notifications.notifications.view", "View Notifications", "notifications", "view", "notifications", "View delivery logs and notification history"},
		{"notifications.notifications.view_own", "View Own Notifications", "notifications", "view_own", "notifications", "View own notification history"},
		{"notifications.notifications.change", "Change Notifications", "notifications", "change", "notifications", "Modify notification settings"},
		{"notifications.notifications.delete", "Delete Notifications", "notifications", "delete", "notifications", "Delete notifications"},
		{"notifications.notifications.manage", "Manage Notifications", "notifications", "manage", "notifications", "Full notification management"},

		// Templates
		{"notifications.templates.add", "Add Templates", "templates", "add", "templates", "Create notification templates"},
		{"notifications.templates.view", "View Templates", "templates", "view", "templates", "View notification templates"},
		{"notifications.templates.change", "Change Templates", "templates", "change", "templates", "Update notification templates"},
		{"notifications.templates.delete", "Delete Templates", "templates", "delete", "templates", "Delete notification templates"},
		{"notifications.templates.manage", "Manage Templates", "templates", "manage", "templates", "Full template management including test sends"},

		// Providers
		{"notifications.providers.view", "View Providers", "providers", "view", "providers", "View provider configurations"},
		{"notifications.providers.change", "Change Providers", "providers", "change", "providers", "Update provider configurations"},
		{"notifications.providers.manage", "Manage Providers", "providers", "manage", "providers", "Full provider management"},

		// Credits
		{"notifications.credits.view", "View Credits", "credits", "view", "credits", "View credit balance"},
		{"notifications.credits.manage", "Manage Credits", "credits", "manage", "credits", "Manage credit transactions"},

		// Billing
		{"notifications.billing.view", "View Billing", "billing", "view", "billing", "View balance and transaction history"},
		{"notifications.billing.manage", "Manage Billing", "billing", "manage", "billing", "Initiate top-ups and manage billing"},

		// Delivery Logs
		{"notifications.delivery_logs.view", "View Delivery Logs", "delivery_logs", "view", "delivery_logs", "View delivery logs"},
		{"notifications.delivery_logs.view_own", "View Own Delivery Logs", "delivery_logs", "view_own", "delivery_logs", "View own delivery logs"},
		{"notifications.delivery_logs.delete", "Delete Delivery Logs", "delivery_logs", "delete", "delivery_logs", "Delete delivery logs"},

		// Config / Settings
		{"notifications.config.view", "View Config", "config", "view", "config", "View tenant settings and configuration"},
		{"notifications.config.change", "Change Config", "config", "change", "config", "Update tenant settings and configuration"},
		{"notifications.config.manage", "Manage Config", "config", "manage", "config", "Full configuration management"},

		// Users
		{"notifications.users.view", "View Users", "users", "view", "users", "View users in tenant"},
		{"notifications.users.view_own", "View Own Profile", "users", "view_own", "users", "View own user profile"},
		{"notifications.users.change", "Change Users", "users", "change", "users", "Update user details"},
		{"notifications.users.manage", "Manage Users", "users", "manage", "users", "Manage user roles and assignments"},

		// Analytics
		{"notifications.analytics.view", "View Analytics", "analytics", "view", "analytics", "View delivery analytics"},
		{"notifications.analytics.manage", "Manage Analytics", "analytics", "manage", "analytics", "Export analytics data"},

		// Platform (superuser only)
		{"notifications.platform.providers", "Platform Providers", "platform", "manage", "providers", "Manage platform-level providers"},
		{"notifications.platform.billing", "Platform Billing", "platform", "manage", "billing", "Manage platform billing settings"},
	}

	for _, p := range perms {
		id := notifPermUUID(p.code)
		existing, _ := client.NotificationPermission.Query().
			Where(notificationpermission.PermissionCode(p.code)).
			Only(ctx)
		if existing != nil {
			_, _ = existing.Update().
				SetName(p.name).
				SetModule(p.module).
				SetAction(p.action).
				SetResource(p.resource).
				SetDescription(p.description).
				Save(ctx)
			continue
		}

		err := client.NotificationPermission.Create().
			SetID(id).
			SetPermissionCode(p.code).
			SetName(p.name).
			SetModule(p.module).
			SetAction(p.action).
			SetResource(p.resource).
			SetDescription(p.description).
			Exec(ctx)
		if err == nil {
			fmt.Printf("    notification permission: %s\n", p.code)
		} else {
			fmt.Printf("    ! notification permission %s: %v\n", p.code, err)
		}
	}
}

// notifRoleUUID generates a deterministic UUID for notification RBAC roles.
func notifRoleUUID(tenantID uuid.UUID, roleCode string) uuid.UUID {
	hash := sha256.Sum256([]byte("notif_role:" + tenantID.String() + ":" + roleCode))
	return uuid.UUID(hash[:16])
}

// seedNotificationRoles creates the new RBAC notification roles for a tenant.
func seedNotificationRoles(ctx context.Context, client *ent.Client, tenantID uuid.UUID) {
	fmt.Printf("  seeding notification roles for tenant %s...\n", tenantID)

	type roleDef struct {
		code        string
		name        string
		description string
	}

	roles := []roleDef{
		{"notifications_admin", "Notifications Admin", "Full administrative access to all notification features"},
		{"operator", "Operator", "Can send notifications, manage templates and providers"},
		{"viewer", "Viewer", "Read-only access to notifications, templates, and analytics"},
	}

	for _, r := range roles {
		id := notifRoleUUID(tenantID, r.code)
		existing, _ := client.NotificationRole.Query().
			Where(
				notificationrole.TenantID(tenantID),
				notificationrole.RoleCode(r.code),
			).Only(ctx)

		if existing != nil {
			_, _ = existing.Update().
				SetName(r.name).
				SetDescription(r.description).
				SetIsSystemRole(true).
				Save(ctx)
			fmt.Printf("    notification role (updated): %s\n", r.code)
			continue
		}

		err := client.NotificationRole.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetRoleCode(r.code).
			SetName(r.name).
			SetDescription(r.description).
			SetIsSystemRole(true).
			Exec(ctx)
		if err == nil {
			fmt.Printf("    notification role: %s\n", r.code)
		} else {
			fmt.Printf("    ! notification role %s: %v\n", r.code, err)
		}
	}
}

// seedNotificationRolePermissions attaches permissions to notification roles.
func seedNotificationRolePermissions(ctx context.Context, client *ent.Client) {
	fmt.Println("  seeding notification role-permission mappings...")

	// Define which permissions each role gets
	rolePermissions := map[string][]string{
		"viewer": {
			"notifications.notifications.view_own",
			"notifications.templates.view",
			"notifications.providers.view",
			"notifications.credits.view",
			"notifications.billing.view",
			"notifications.delivery_logs.view_own",
			"notifications.config.view",
			"notifications.users.view_own",
			"notifications.analytics.view",
		},
		"operator": {
			"notifications.notifications.add",
			"notifications.notifications.view",
			"notifications.notifications.view_own",
			"notifications.notifications.change",
			"notifications.templates.add",
			"notifications.templates.view",
			"notifications.templates.change",
			"notifications.templates.manage",
			"notifications.providers.view",
			"notifications.providers.change",
			"notifications.credits.view",
			"notifications.billing.view",
			"notifications.delivery_logs.view",
			"notifications.delivery_logs.view_own",
			"notifications.config.view",
			"notifications.users.view",
			"notifications.users.view_own",
			"notifications.analytics.view",
			"notifications.analytics.manage",
		},
		"notifications_admin": {
			"notifications.notifications.add",
			"notifications.notifications.view",
			"notifications.notifications.view_own",
			"notifications.notifications.change",
			"notifications.notifications.delete",
			"notifications.notifications.manage",
			"notifications.templates.add",
			"notifications.templates.view",
			"notifications.templates.change",
			"notifications.templates.delete",
			"notifications.templates.manage",
			"notifications.providers.view",
			"notifications.providers.change",
			"notifications.providers.manage",
			"notifications.credits.view",
			"notifications.credits.manage",
			"notifications.billing.view",
			"notifications.billing.manage",
			"notifications.delivery_logs.view",
			"notifications.delivery_logs.view_own",
			"notifications.delivery_logs.delete",
			"notifications.config.view",
			"notifications.config.change",
			"notifications.config.manage",
			"notifications.users.view",
			"notifications.users.view_own",
			"notifications.users.change",
			"notifications.users.manage",
			"notifications.analytics.view",
			"notifications.analytics.manage",
		},
	}

	// Get all tenants that have notification roles
	tenantRoles, err := client.NotificationRole.Query().All(ctx)
	if err != nil {
		fmt.Printf("    ! failed to list notification roles: %v\n", err)
		return
	}

	for _, role := range tenantRoles {
		permCodes, ok := rolePermissions[role.RoleCode]
		if !ok {
			continue
		}

		permIDs := make([]uuid.UUID, 0, len(permCodes))
		for _, code := range permCodes {
			permIDs = append(permIDs, notifPermUUID(code))
		}

		err := role.Update().
			ClearPermissions().
			AddPermissionIDs(permIDs...).
			Exec(ctx)
		if err != nil {
			fmt.Printf("    ! notification role-perms %s (tenant %s): %v\n", role.RoleCode, role.TenantID, err)
		} else {
			fmt.Printf("    notification role-perms: %s (%d permissions)\n", role.RoleCode, len(permIDs))
		}
	}
}

// seedRateLimitConfigs creates default rate limit configurations.
func seedRateLimitConfigs(ctx context.Context, client *ent.Client) {
	fmt.Println("  seeding rate limit configs...")

	type rlConfig struct {
		serviceName      string
		keyType          string
		endpointPattern  string
		requestsPerWin   int
		windowSecs       int
		burstMultiplier  float64
		description      string
	}

	configs := []rlConfig{
		{"notifications-api", "tenant", "*", 1000, 60, 1.5, "Default tenant rate limit: 1000 requests/min"},
		{"notifications-api", "ip", "*", 100, 60, 2.0, "Default IP rate limit: 100 requests/min"},
		{"notifications-api", "tenant", "/api/v1/*/notifications/messages", 500, 60, 1.2, "Notification send rate limit: 500/min per tenant"},
		{"notifications-api", "user", "*", 200, 60, 1.5, "Default per-user rate limit: 200 requests/min"},
		{"notifications-api", "global", "*", 10000, 60, 1.5, "Global platform rate limit: 10000 requests/min"},
	}

	for _, c := range configs {
		existing, _ := client.RateLimitConfig.Query().
			Where(
				ratelimitconfig.ServiceName(c.serviceName),
				ratelimitconfig.KeyType(c.keyType),
				ratelimitconfig.EndpointPattern(c.endpointPattern),
			).Only(ctx)

		if existing != nil {
			_, _ = existing.Update().
				SetRequestsPerWindow(c.requestsPerWin).
				SetWindowSeconds(c.windowSecs).
				SetBurstMultiplier(c.burstMultiplier).
				SetDescription(c.description).
				SetIsActive(true).
				Save(ctx)
			continue
		}

		err := client.RateLimitConfig.Create().
			SetServiceName(c.serviceName).
			SetKeyType(c.keyType).
			SetEndpointPattern(c.endpointPattern).
			SetRequestsPerWindow(c.requestsPerWin).
			SetWindowSeconds(c.windowSecs).
			SetBurstMultiplier(c.burstMultiplier).
			SetDescription(c.description).
			SetIsActive(true).
			Exec(ctx)
		if err == nil {
			fmt.Printf("    rate limit: %s/%s/%s\n", c.serviceName, c.keyType, c.endpointPattern)
		} else {
			fmt.Printf("    ! rate limit %s/%s/%s: %v\n", c.serviceName, c.keyType, c.endpointPattern, err)
		}
	}
}

// seedServiceConfigs creates default service configuration entries.
func seedServiceConfigs(ctx context.Context, client *ent.Client) {
	fmt.Println("  seeding service configs...")

	type svcConfig struct {
		configKey   string
		configValue string
		configType  string
		description string
		isSecret    bool
	}

	// Platform-level defaults (tenant_id = nil)
	configs := []svcConfig{
		{"notifications.default_channel", "email", "string", "Default notification channel when none specified", false},
		{"notifications.max_retries", "3", "int", "Maximum retry attempts for failed notifications", false},
		{"notifications.retry_delay_seconds", "60", "int", "Delay in seconds between retry attempts", false},
		{"notifications.sms_enabled", "true", "bool", "Whether SMS notifications are enabled platform-wide", false},
		{"notifications.whatsapp_enabled", "true", "bool", "Whether WhatsApp notifications are enabled platform-wide", false},
		{"notifications.push_enabled", "true", "bool", "Whether push notifications are enabled platform-wide", false},
		{"notifications.email_enabled", "true", "bool", "Whether email notifications are enabled platform-wide", false},
		{"notifications.max_batch_size", "100", "int", "Maximum batch size for bulk notification sends", false},
		{"notifications.template_cache_ttl_seconds", "300", "int", "Template cache TTL in seconds", false},
		{"notifications.daily_send_limit", "10000", "int", "Default daily send limit per tenant", false},
		{"notifications.webhook_timeout_seconds", "10", "int", "Timeout for webhook deliveries", false},
		{"notifications.log_retention_days", "90", "int", "Number of days to retain delivery logs", false},
	}

	for _, c := range configs {
		existing, _ := client.ServiceConfig.Query().
			Where(
				serviceconfig.ConfigKey(c.configKey),
				serviceconfig.TenantIDIsNil(),
			).Only(ctx)

		if existing != nil {
			_, _ = existing.Update().
				SetConfigValue(c.configValue).
				SetConfigType(c.configType).
				SetDescription(c.description).
				SetIsSecret(c.isSecret).
				Save(ctx)
			continue
		}

		err := client.ServiceConfig.Create().
			SetConfigKey(c.configKey).
			SetConfigValue(c.configValue).
			SetConfigType(c.configType).
			SetDescription(c.description).
			SetIsSecret(c.isSecret).
			Exec(ctx)
		if err == nil {
			fmt.Printf("    service config: %s = %s\n", c.configKey, c.configValue)
		} else {
			fmt.Printf("    ! service config %s: %v\n", c.configKey, err)
		}
	}
}

// seedPlatformAdmin seeds a platform admin user for the codevertex tenant.
func seedPlatformAdmin(ctx context.Context, client *ent.Client, syncer *tenantmodule.Syncer, adminUserIDStr string) {
	adminUserID, err := uuid.Parse(adminUserIDStr)
	if err != nil {
		fmt.Printf("  ! invalid SEED_PLATFORM_ADMIN_USER_ID: %v\n", err)
		return
	}

	// Ensure codevertex tenant is synced
	tenantID, err := syncer.SyncTenant(ctx, "codevertex")
	if err != nil {
		fmt.Printf("  ! failed to sync codevertex tenant for admin: %v\n", err)
		return
	}

	// Check if user already exists
	existing, _ := client.User.Get(ctx, adminUserID)
	if existing != nil {
		fmt.Printf("  platform admin already exists: %s\n", adminUserID)
		return
	}

	// Ensure superuser and admin roles exist
	for _, roleID := range []string{"superuser", "admin"} {
		if _, err := client.Role.Get(ctx, roleID); err != nil {
			fmt.Printf("  ! role %s not found, run seedRoles first\n", roleID)
			return
		}
	}

	err = client.User.Create().
		SetID(adminUserID).
		SetTenantID(tenantID).
		SetAuthServiceUserID(adminUserID).
		SetEmail("admin@codevertexitsolutions.com").
		SetFullName("Platform Admin").
		SetStatus("active").
		SetSyncStatus("synced").
		SetLocale("en").
		AddRoleIDs("superuser", "admin").
		Exec(ctx)
	if err != nil {
		fmt.Printf("  ! platform admin create: %v\n", err)
	} else {
		fmt.Printf("  platform admin seeded: %s\n", adminUserID)
	}

	// Also assign the notifications_admin role in the new RBAC system
	adminRole, _ := client.NotificationRole.Query().
		Where(
			notificationrole.TenantID(tenantID),
			notificationrole.RoleCode("notifications_admin"),
		).Only(ctx)
	if adminRole != nil {
		_ = client.UserRoleAssignment.Create().
			SetTenantID(tenantID).
			SetUserID(adminUserID).
			SetRoleID(adminRole.ID).
			SetAssignedBy(adminUserID).
			Exec(ctx)
		fmt.Printf("  platform admin assigned notifications_admin role\n")
	}
}
