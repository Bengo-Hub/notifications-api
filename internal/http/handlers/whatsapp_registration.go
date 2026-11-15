package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	httpware "github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	"github.com/bengobox/notifications-api/internal/providers"
	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// Registering, re-registering and releasing WhatsApp numbers so a number can be disconnected and
// connected again any number of times (owner, 15 November 2026). Meta keeps a number's
// platform_type at CLOUD_API after a deregister, and two-step verification keeps its PIN, so:
// registration is decided on the live status, the PIN we set is kept and sent again, and a number
// or account shared with the portfolio is never released by one tenant's Disconnect.

// platformTierTenant is the tenant_id of the platform tier rows (Platform > Providers). They hold
// the platform's own number but are not a tenant link.
const platformTierTenant = "platform"

// registeredOnCloudAPI reports a number that is registered and connected on Cloud API right now.
// platform_type alone is not enough: it stays CLOUD_API after a deregister, and skipping register
// then left every send failing with 133010 "Account does not exist in Cloud API".
func registeredOnCloudAPI(platformType, status string) bool {
	return strings.EqualFold(strings.TrimSpace(platformType), "CLOUD_API") &&
		strings.EqualFold(strings.TrimSpace(status), "CONNECTED")
}

// errNoPIN is returned when no registration PIN could be made.
var errNoPIN = errors.New("could not create a registration PIN")

// registerNumber registers the number on Cloud API with the tenant's stored PIN, or a new one when
// none is stored, and returns the PIN used so the caller saves it.
func (h *WhatsAppEmbeddedSignupHandler) registerNumber(ctx context.Context, tenantID, apiVersion, phoneNumberID, token string) (string, error) {
	pin := ""
	if h.storedPIN != nil {
		pin = strings.TrimSpace(h.storedPIN(ctx, tenantID))
	}
	if !isPIN(pin) {
		var err error
		if pin, err = h.newPIN(); err != nil {
			return "", errNoPIN
		}
	}
	err := h.graphPost(ctx, apiVersion, phoneNumberID+"/register", token, map[string]any{
		"messaging_product": "whatsapp",
		"pin":               pin,
	})
	return pin, err
}

// isPIN accepts Meta's six digit two-step verification PIN.
func isPIN(s string) bool {
	return len(s) == 6 && isGraphID(s)
}

// actingOrPlatformTenant is the tenant a platform owner acts for, or the platform tenant when none
// is named (All Tenants in the switcher).
func (h *WhatsAppEmbeddedSignupHandler) actingOrPlatformTenant(r *http.Request) string {
	if t := resolveActingTenantID(r); t != "" {
		return t
	}
	return h.platformID
}

// linkedNumber is the number, token and Graph version to act on for a tenant. The platform tenant
// falls back to the platform tier, like its sends do.
func (h *WhatsAppEmbeddedSignupHandler) linkedNumber(ctx context.Context, tenantID string) (phoneID, token, version string) {
	if h.conn != nil {
		phoneID, _, token, version = h.conn(ctx, tenantID)
	}
	if tenantID != "" && tenantID == h.platformID && h.creds != nil {
		if creds, err := h.creds(ctx); err == nil {
			if phoneID == "" {
				phoneID = strings.TrimSpace(creds["phone_number_id"])
			}
			if token == "" {
				token = strings.TrimSpace(creds["access_token"])
			}
			if version == "" {
				version = creds["api_version"]
			}
		}
	}
	return phoneID, token, version
}

// Reregister registers the tenant's linked number on Cloud API again without disconnecting it,
// for a number Meta reports as not registered (133010 on a send). It sends the stored PIN, or sets
// a new one when none is stored, and saves the PIN used.
//
// Platform owners only, for the tenant named in X-Tenant-ID or the platform tenant.
//
// @Summary Register a linked WhatsApp number on Cloud API again
// @Tags WhatsApp
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 403,404,422,500,502,503 {object} errorResponse
// @Security bearerAuth
// @Router /billing/whatsapp/reregister [post]
func (h *WhatsAppEmbeddedSignupHandler) Reregister(w http.ResponseWriter, r *http.Request) {
	if !httpware.IsPlatformOwner(r.Context()) {
		jsonError(w, http.StatusForbidden, "only a platform owner can register a number again")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), signupTimeout)
	defer cancel()
	tenantID := h.actingOrPlatformTenant(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "choose the tenant first")
		return
	}
	phoneID, token, version := h.linkedNumber(ctx, tenantID)
	if phoneID == "" {
		jsonError(w, http.StatusNotFound, "no WhatsApp number is linked for this tenant: link or connect it first")
		return
	}
	if token == "" {
		jsonError(w, http.StatusServiceUnavailable, "no access token is available for this number: link it again from our portfolio, or reconnect it with Embedded Signup")
		return
	}
	pin, err := h.registerNumber(ctx, tenantID, whatsapp.GraphVersion(version), phoneID, token)
	if err != nil {
		h.log.Error("whatsapp reregister: registration failed", zap.String("tenant_id", tenantID), zap.String("phone_number_id", phoneID), zap.Error(err))
		writeMetaRefusal(w, err, token, "Meta refused to register the number for Cloud API", "")
		return
	}
	encPIN, err := h.encrypt(pin)
	if err == nil {
		err = h.save(ctx, tenantID, []signupSetting{{key: "registration_pin", value: encPIN, secret: true}})
	}
	if err != nil {
		h.log.Error("whatsapp reregister: could not save the PIN", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "the number was registered but its PIN could not be saved, contact support")
		return
	}
	pcfg.InvalidateCache()
	h.log.Info("whatsapp number registered again", zap.String("tenant_id", tenantID), zap.String("phone_number_id", phoneID))
	jsonResponse(w, http.StatusOK, map[string]any{
		"message":         "WhatsApp number registered on Cloud API",
		"phone_number_id": phoneID,
	})
}

// RegistrationStatus reports the tenant's number as Meta sees it now, so the settings page can
// tell a connected number from one that is linked here but not registered on Cloud API.
//
// @Summary Live Cloud API registration state of the tenant's WhatsApp number
// @Tags WhatsApp
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /billing/whatsapp/registration [get]
func (h *WhatsAppEmbeddedSignupHandler) RegistrationStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), graphCallTimeout)
	defer cancel()
	tenantID := resolveActingTenantID(r)
	if tenantID == "" && httpware.IsPlatformOwner(r.Context()) {
		tenantID = h.platformID
	}
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}
	phoneID, token, version := h.linkedNumber(ctx, tenantID)
	out := map[string]any{"phone_number_id": phoneID, "known": false, "registered": false}
	if phoneID == "" || token == "" {
		jsonResponse(w, http.StatusOK, out)
		return
	}
	var phone struct {
		PlatformType           string `json:"platform_type"`
		Status                 string `json:"status"`
		CodeVerificationStatus string `json:"code_verification_status"`
	}
	if err := h.graphGetJSON(ctx, whatsapp.GraphVersion(version), phoneID+"?fields=platform_type,status,code_verification_status", token, &phone); err != nil {
		h.log.Warn("whatsapp registration status: Meta read failed", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonResponse(w, http.StatusOK, out)
		return
	}
	registered := registeredOnCloudAPI(phone.PlatformType, phone.Status)
	out["known"] = true
	out["status"] = phone.Status
	out["platform_type"] = phone.PlatformType
	out["code_verification_status"] = phone.CodeVerificationStatus
	out["registered"] = registered
	// A number that is not registered and not verified needs a code before it can be registered.
	cvs := strings.ToUpper(strings.TrimSpace(phone.CodeVerificationStatus))
	out["needs_verification"] = !registered && cvs != "" && cvs != "VERIFIED"
	jsonResponse(w, http.StatusOK, out)
}

// disconnectPlan is what a Disconnect releases on Meta, with notes saying what it kept.
type disconnectPlan struct {
	deregister  bool
	unsubscribe bool
	notes       []string
}

// disconnectPlan decides what a Disconnect releases on Meta:
//   - the platform tenant's own number is never released: it keeps serving platform messages
//     through the platform tier, and only its link here is removed;
//   - a number another tenant (or the platform tier) also holds is never deregistered, and a
//     WhatsApp account another tenant or the platform also uses is never unsubscribed;
//   - a number in a shared portfolio account is deregistered only when release=true is asked
//     for; release=false keeps any number registered, so it can be linked again at once;
//   - with no release parameter a tenant's own (Embedded Signup) account is released as before.
func (h *WhatsAppEmbeddedSignupHandler) disconnectPlan(ctx context.Context, r *http.Request, tenantID, phoneID, wabaID string) disconnectPlan {
	if tenantID == h.platformID {
		return disconnectPlan{notes: []string{"the platform's own number stays registered on Meta and keeps serving platform messages; only its link here was removed"}}
	}
	phoneShared, wabaShared := false, false
	if h.creds != nil {
		if creds, err := h.creds(ctx); err == nil {
			if p := strings.TrimSpace(creds["phone_number_id"]); p != "" && p == phoneID {
				phoneShared = true
			}
			if a := strings.TrimSpace(creds["waba_id"]); a != "" && a == wabaID {
				wabaShared = true
			}
		}
	}
	if h.sharedUse != nil {
		ps, ws, err := h.sharedUse(ctx, tenantID, phoneID, wabaID)
		if err != nil {
			// Unsure means shared: releasing a number someone else uses is the worse mistake.
			h.log.Warn("whatsapp disconnect: could not check shared use, keeping the number and account on Meta", zap.String("tenant_id", tenantID), zap.Error(err))
			ps, ws = true, true
		}
		phoneShared = phoneShared || ps
		wabaShared = wabaShared || ws
	}
	release := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("release")))
	asked := release == "true" || release == "1"
	plan := disconnectPlan{unsubscribe: !wabaShared}
	switch {
	case phoneShared:
		plan.notes = append(plan.notes, "the number is also used elsewhere, so it stays registered on Meta")
	case asked:
		plan.deregister = true
	case release == "" && !wabaShared:
		plan.deregister = true
	default:
		plan.notes = append(plan.notes, "the number stays registered on Meta, so it can be linked again at once")
	}
	if wabaShared {
		plan.notes = append(plan.notes, "the WhatsApp account is shared with other numbers, so our webhook stays subscribed to it")
	}
	return plan
}

// connectionSharedUse looks for the phone number ID and WhatsApp account ID on any other tenant's
// rows or on the platform tier.
func (h *WhatsAppEmbeddedSignupHandler) connectionSharedUse(ctx context.Context, tenantID, phoneNumberID, wabaID string) (bool, bool, error) {
	held := func(key, value string) (bool, error) {
		if value == "" {
			return false, nil
		}
		return h.client.ProviderSetting.Query().
			Where(
				providersetting.TenantIDNEQ(tenantID),
				providersetting.ProviderType("whatsapp"),
				providersetting.ProviderName("meta_cloud"),
				providersetting.Key(key),
				providersetting.Value(value),
			).
			Exist(ctx)
	}
	phone, err := held("phone_number_id", phoneNumberID)
	if err != nil {
		return false, false, err
	}
	waba, err := held("waba_id", wabaID)
	if err != nil {
		return false, false, err
	}
	return phone, waba, nil
}

// numberLinkedElsewhere reports a phone number ID saved on another tenant, or a WhatsApp account
// ID saved on another tenant that is not a portfolio link. Numbers of different tenants share
// the portfolio accounts (the platform's own account among them), so an account held only by
// managed links (token_source=platform) is no conflict. The platform tier is not a tenant link.
// An empty wabaID checks the number only.
func (h *WhatsAppEmbeddedSignupHandler) numberLinkedElsewhere(ctx context.Context, tenantID, phoneNumberID, wabaID string) (bool, error) {
	others := []string{tenantID, platformTierTenant}
	taken, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDNotIn(others...),
			providersetting.ProviderType("whatsapp"),
			providersetting.ProviderName("meta_cloud"),
			providersetting.Key("phone_number_id"),
			providersetting.Value(phoneNumberID),
		).
		Exist(ctx)
	if err != nil || taken || wabaID == "" {
		return taken, err
	}
	holders, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDNotIn(others...),
			providersetting.ProviderType("whatsapp"),
			providersetting.ProviderName("meta_cloud"),
			providersetting.Key("waba_id"),
			providersetting.Value(wabaID),
		).
		Select(providersetting.FieldTenantID).
		Strings(ctx)
	if err != nil || len(holders) == 0 {
		return false, err
	}
	managed, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDIn(holders...),
			providersetting.ProviderType("whatsapp"),
			providersetting.ProviderName("meta_cloud"),
			providersetting.Key(providers.TokenSourceKey),
			providersetting.Value(providers.TokenSourcePlatform),
		).
		Select(providersetting.FieldTenantID).
		Strings(ctx)
	if err != nil {
		return false, err
	}
	return wabaHeldOutsidePortfolio(holders, managed), nil
}

// wabaHeldOutsidePortfolio reports a holder of the account that is not a portfolio link.
func wabaHeldOutsidePortfolio(holders, managed []string) bool {
	isManaged := make(map[string]bool, len(managed))
	for _, t := range managed {
		isManaged[t] = true
	}
	for _, t := range holders {
		if !isManaged[t] {
			return true
		}
	}
	return false
}
