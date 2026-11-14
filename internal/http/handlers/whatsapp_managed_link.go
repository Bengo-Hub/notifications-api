package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	httpware "github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/providers"
	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// managedLinkRequest names a number a platform owner set up in WhatsApp Manager.
type managedLinkRequest struct {
	PhoneNumberID string `json:"phone_number_id"`
	WABAID        string `json:"waba_id"`
}

// systemUserHint ends every message about Meta refusing a managed link call: almost always the
// platform system user is not assigned to the account.
const systemUserHint = "check in WhatsApp Manager that the account is in the Codevertex portfolio and that the Codevertex system user is assigned to it with full control"

// ManagedLink links a number from the Codevertex verified portfolio to a tenant (owner decision
// 2026-11-13). Embedded Signup never lets the business that owns the app be chosen, so for a
// tenant like Hadia a platform owner creates a WhatsApp account inside the verified portfolio in
// WhatsApp Manager, adds the number, attaches the card and assigns the system user, then links it
// here. The tenant's rows keep its own number and account IDs with token_source=platform, so its
// sends and syncs use the platform system user's token; no tenant token is stored.
//
// Platform owners only, acting for the tenant named in X-Tenant-ID.
//
// @Summary Link a number from the Codevertex portfolio to a tenant
// @Description Platform owners only. Checks the number belongs to the WhatsApp account, subscribes the webhook, registers the number when needed and saves it for the tenant named in X-Tenant-ID.
// @Tags WhatsApp
// @Accept json
// @Produce json
// @Param body body managedLinkRequest true "Phone number ID and WhatsApp account ID"
// @Success 200 {object} map[string]interface{}
// @Failure 400,403,409,502,503 {object} errorResponse
// @Security bearerAuth
// @Router /billing/whatsapp/managed-link [post]
func (h *WhatsAppEmbeddedSignupHandler) ManagedLink(w http.ResponseWriter, r *http.Request) {
	if !httpware.IsPlatformOwner(r.Context()) {
		jsonError(w, http.StatusForbidden, "only a platform owner can link a number from the Codevertex portfolio")
		return
	}
	// Like Complete, the calls to Meta run to the end even if the browser gives up first.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), signupTimeout)
	defer cancel()
	tenantID := resolveActingTenantID(r)
	if tenantID == "" || tenantID == h.platformID {
		jsonError(w, http.StatusBadRequest, "choose the tenant to link the number to first")
		return
	}

	var req managedLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "phone_number_id and waba_id are required")
		return
	}
	req.PhoneNumberID = strings.TrimSpace(req.PhoneNumberID)
	req.WABAID = strings.TrimSpace(req.WABAID)
	if !isGraphID(req.PhoneNumberID) || !isGraphID(req.WABAID) {
		jsonError(w, http.StatusBadRequest, "phone_number_id and waba_id are required and must be the numeric IDs from WhatsApp Manager")
		return
	}

	creds, err := h.creds(ctx)
	if err != nil {
		h.log.Error("managed link: could not load platform Meta settings", zap.Error(err))
	}
	token := strings.TrimSpace(creds["access_token"])
	if token == "" {
		jsonError(w, http.StatusServiceUnavailable,
			"the platform WhatsApp access token is not set: add it under Platform > Providers > WhatsApp (Meta Cloud)")
		return
	}
	if req.WABAID == strings.TrimSpace(creds["waba_id"]) || req.PhoneNumberID == strings.TrimSpace(creds["phone_number_id"]) {
		jsonError(w, http.StatusConflict, "this is the platform's own WhatsApp account or number, it cannot be linked to a tenant")
		return
	}
	if h.linkedElsewhere != nil {
		taken, err := h.linkedElsewhere(ctx, tenantID, req.PhoneNumberID, req.WABAID)
		if err != nil {
			h.log.Error("managed link: checking existing links failed", zap.Error(err))
			jsonError(w, http.StatusInternalServerError, "could not check whether the number is already linked, try again")
			return
		}
		if taken {
			jsonError(w, http.StatusConflict, "this number or WhatsApp account is already linked to another tenant: disconnect it there first")
			return
		}
	}
	apiVersion := whatsapp.GraphVersion(creds["api_version"])

	var phone struct {
		DisplayPhoneNumber string `json:"display_phone_number"`
		VerifiedName       string `json:"verified_name"`
		PlatformType       string `json:"platform_type"`
		Status             string `json:"status"`
	}
	if err := h.graphGetJSON(ctx, apiVersion, req.PhoneNumberID+"?fields=display_phone_number,verified_name,platform_type,status", token, &phone); err != nil {
		h.log.Warn("managed link: reading the number failed", zap.String("phone_number_id", req.PhoneNumberID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, "Meta refused to show the number: "+err.Error()+"; "+systemUserHint)
		return
	}
	var numbers struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := h.graphGetJSON(ctx, apiVersion, req.WABAID+"/phone_numbers?fields=id&limit=100", token, &numbers); err != nil {
		h.log.Warn("managed link: listing the account's numbers failed", zap.String("waba_id", req.WABAID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, "Meta refused to list the WhatsApp account's numbers: "+err.Error()+"; "+systemUserHint)
		return
	}
	inAccount := false
	for _, n := range numbers.Data {
		if n.ID == req.PhoneNumberID {
			inAccount = true
			break
		}
	}
	if !inAccount {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("phone number %s is not in WhatsApp account %s: check both IDs in WhatsApp Manager", req.PhoneNumberID, req.WABAID))
		return
	}

	if err := h.graphPost(ctx, apiVersion, req.WABAID+"/subscribed_apps", token, nil); err != nil {
		h.log.Error("managed link: webhook subscription failed", zap.String("waba_id", req.WABAID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, "Meta refused to subscribe our webhook to the WhatsApp account: "+metaMessage(err)+"; "+systemUserHint)
		return
	}

	// A number already on Cloud API is not registered again: a new PIN would be refused against the
	// one set earlier.
	pin := ""
	if phone.PlatformType != "CLOUD_API" {
		if pin, err = h.newPIN(); err != nil {
			jsonError(w, http.StatusInternalServerError, "could not create a registration PIN")
			return
		}
		if err := h.graphPost(ctx, apiVersion, req.PhoneNumberID+"/register", token, map[string]any{
			"messaging_product": "whatsapp",
			"pin":               pin,
		}); err != nil {
			h.log.Error("managed link: phone registration failed", zap.String("phone_number_id", req.PhoneNumberID), zap.Error(err))
			jsonError(w, http.StatusBadGateway, "Meta refused to register the number for Cloud API: "+metaMessage(err))
			return
		}
	}

	rows := []signupSetting{
		{key: "phone_number_id", value: req.PhoneNumberID},
		{key: "waba_id", value: req.WABAID},
		{key: providers.TokenSourceKey, value: providers.TokenSourcePlatform},
	}
	if pin != "" {
		encPIN, err := h.encrypt(pin)
		if err != nil {
			h.log.Error("managed link: could not encrypt the registration PIN", zap.String("tenant_id", tenantID), zap.Error(err))
			jsonError(w, http.StatusInternalServerError, "the number was registered but its PIN could not be saved, contact support")
			return
		}
		rows = append(rows, signupSetting{key: "registration_pin", value: encPIN, secret: true})
	}
	if err := h.save(ctx, tenantID, rows); err != nil {
		h.log.Error("managed link: failed to store tenant settings", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "linked on Meta but failed to save the number, try again")
		return
	}
	// An old tenant token from an earlier Embedded Signup is no longer used (the marker wins) and
	// is removed so nothing can pick it up later.
	if h.removeKeys != nil {
		if err := h.removeKeys(ctx, tenantID, "access_token"); err != nil {
			h.log.Warn("managed link: could not remove the old tenant access token", zap.String("tenant_id", tenantID), zap.Error(err))
		}
	}
	pcfg.InvalidateCache()

	h.log.Info("whatsapp managed link completed",
		zap.String("tenant_id", tenantID), zap.String("waba_id", req.WABAID), zap.String("phone_number_id", req.PhoneNumberID))
	if h.autoSync != nil {
		go h.autoSync(context.WithoutCancel(r.Context()), tenantID)
	}
	if h.profileSync != nil {
		go h.profileSync(context.WithoutCancel(r.Context()), tenantID)
	}
	resp := map[string]any{
		"message":              "WhatsApp number linked",
		"phone_number_id":      req.PhoneNumberID,
		"waba_id":              req.WABAID,
		"display_phone_number": phone.DisplayPhoneNumber,
		"verified_name":        phone.VerifiedName,
		"token_source":         providers.TokenSourcePlatform,
	}
	if has, known := h.hasPaymentMethod(ctx, apiVersion, req.WABAID, token); known && !has {
		h.log.Warn("managed link: the WhatsApp account has no payment method",
			zap.String("tenant_id", tenantID), zap.String("waba_id", req.WABAID))
		resp["payment_method_missing"] = true
	}
	jsonResponse(w, http.StatusOK, resp)
}

// managedWhatsAppEditRefusal stops a tenant from using the settings form to borrow the platform
// token: only a platform owner may set token_source, and on a managed link only a platform owner
// may change the number or account (otherwise a tenant could point the platform token at any
// number in the portfolio). It returns the refusal message, or "" when the edit is allowed.
func managedWhatsAppEditRefusal(providerType, providerName string, platformOwner bool, existing, settings map[string]string) string {
	if platformOwner || providerType != "whatsapp" || providerName != "meta_cloud" {
		return ""
	}
	changed := func(k string) bool {
		v, ok := settings[k]
		v = strings.TrimSpace(v)
		return ok && v != "" && v != "••••••••" && v != strings.TrimSpace(existing[k])
	}
	if changed(providers.TokenSourceKey) {
		return "only a platform owner can change how this WhatsApp number is connected"
	}
	if strings.TrimSpace(existing[providers.TokenSourceKey]) == providers.TokenSourcePlatform &&
		(changed("phone_number_id") || changed("waba_id")) {
		return "this WhatsApp number was linked by Codevertex: ask Codevertex support to change it"
	}
	return ""
}

// manualWhatsAppNumberRefusal stops the settings form from saving a tenant's phone number ID with
// nothing to use it with: no WhatsApp account, no token and no managed-link marker. That is how
// Hadia's number was saved on 14 November 2026, and every send, template and profile call then
// failed. A platform owner is sent to the portfolio link, which checks the number on Meta and saves
// the account and marker with it; anyone else is told how to connect. The platform's own row is
// left alone (its token is saved on Platform > Providers). Returns "" when the save is allowed.
func manualWhatsAppNumberRefusal(providerType, providerName, tenantID, platformID string, platformOwner bool, existing, settings map[string]string) string {
	if providerType != "whatsapp" || providerName != "meta_cloud" || tenantID == platformID {
		return ""
	}
	value := func(k string) string {
		if v := strings.TrimSpace(settings[k]); v != "" && v != "••••••••" {
			return v
		}
		return strings.TrimSpace(existing[k])
	}
	if value("phone_number_id") == "" || value("waba_id") != "" || value("access_token") != "" ||
		value(providers.TokenSourceKey) == providers.TokenSourcePlatform {
		return ""
	}
	if platformOwner {
		return "a phone number ID on its own cannot send anything: use Link a number from our portfolio above with the phone number ID and the WhatsApp account ID from WhatsApp Manager"
	}
	return "a WhatsApp number cannot be entered by hand: connect it with Embedded Signup (Connect WhatsApp Number), or ask Codevertex support to link a number from our portfolio for you"
}

// isGraphID accepts Meta's numeric object IDs only, so nothing else reaches a Graph URL path.
func isGraphID(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// graphGetJSON GETs a Graph path and decodes the answer into out. A refusal comes back as Meta's
// own error message.
func (h *WhatsAppEmbeddedSignupHandler) graphGetJSON(ctx context.Context, apiVersion, path, accessToken string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/%s/%s", h.graphBase, apiVersion, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := h.http.Do(req)
	if err != nil {
		return errors.New("Meta could not be reached")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return errors.New(graphErrorMessage(resp.Status, body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("Meta's answer could not be read")
	}
	return nil
}

// graphErrorMessage is the error.message of a Graph error body, else the HTTP status.
func graphErrorMessage(status string, body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && strings.TrimSpace(e.Error.Message) != "" {
		return e.Error.Message
	}
	return status
}

// metaMessage pulls Meta's own message out of a graphPost error ("meta graph api path: status:
// body"), falling back to the whole error.
func metaMessage(err error) string {
	s := err.Error()
	if i := strings.Index(s, "{"); i >= 0 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(s[i:]), &e) == nil && e.Error.Message != "" {
			return e.Error.Message
		}
	}
	return s
}
