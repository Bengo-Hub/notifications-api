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
// Platform owners only, acting for the tenant named in X-Tenant-ID. With no tenant named (All
// Tenants in the switcher) the target is the platform tenant itself, so the platform's own number
// can be linked again after a Disconnect (owner, 15 November 2026). Other tenants' numbers may sit
// in the platform's own WhatsApp account (the portfolio); only the platform's own phone number is
// kept for the platform tenant.
//
// A refusal from Meta answers 422 with Meta's own title and message (meta_title, meta_message,
// meta_code, meta_subcode) and a hint when one is known; 502 is kept for Meta not answering.
//
// @Summary Link a number from the Codevertex portfolio to a tenant
// @Description Platform owners only. Checks the number belongs to the WhatsApp account, subscribes the webhook, registers the number when needed and saves it for the tenant named in X-Tenant-ID, or for the platform tenant when none is named.
// @Tags WhatsApp
// @Accept json
// @Produce json
// @Param body body managedLinkRequest true "Phone number ID and WhatsApp account ID"
// @Success 200 {object} map[string]interface{}
// @Failure 400,403,409,422,502,503 {object} errorResponse
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
	if tenantID == "" {
		tenantID = h.platformID
	}
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "choose the tenant to link the number to first")
		return
	}
	toPlatform := tenantID == h.platformID

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
	// The platform's own number belongs to the platform tenant only. Its WhatsApp account is the
	// portfolio, where other tenants' numbers live too, so the account alone is no reason to refuse.
	if !toPlatform && req.PhoneNumberID == strings.TrimSpace(creds["phone_number_id"]) {
		jsonError(w, http.StatusConflict, "this is the platform's own WhatsApp number, it can only be linked to the platform (Codevertex)")
		return
	}
	if h.linkedElsewhere != nil {
		// A number in the platform's own account is checked by its phone number only.
		wabaToCheck := req.WABAID
		if req.WABAID == strings.TrimSpace(creds["waba_id"]) {
			wabaToCheck = ""
		}
		taken, err := h.linkedElsewhere(ctx, tenantID, req.PhoneNumberID, wabaToCheck)
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
	if err := h.graphGetJSON(ctx, apiVersion, req.PhoneNumberID+"?fields=display_phone_number,verified_name,platform_type,status,code_verification_status", token, &phone); err != nil {
		h.log.Warn("managed link: reading the number failed", zap.String("phone_number_id", req.PhoneNumberID), zap.Error(err))
		writeMetaRefusal(w, err, token, "Meta refused to show the number", systemUserHint)
		return
	}
	var numbers struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := h.graphGetJSON(ctx, apiVersion, req.WABAID+"/phone_numbers?fields=id&limit=100", token, &numbers); err != nil {
		h.log.Warn("managed link: listing the account's numbers failed", zap.String("waba_id", req.WABAID), zap.Error(err))
		writeMetaRefusal(w, err, token, "Meta refused to list the WhatsApp account's numbers", systemUserHint)
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
		writeMetaRefusal(w, err, token, "Meta refused to subscribe our webhook to the WhatsApp account", systemUserHint)
		return
	}

	// A number connected on Cloud API is not registered again. The live status decides, not
	// platform_type alone: a deregistered number (after a Disconnect) still reports CLOUD_API, and
	// skipping register then left Urban Loft failing every send with 133010 (15 November 2026).
	pin := ""
	// A number Meta wants verified again (133006) is still linked, so the settings page can verify
	// it with a code and register it in place; the answer says needs_verification.
	var verifyErr error
	if !registeredOnCloudAPI(phone.PlatformType, phone.Status) {
		if pin, err = h.registerNumber(ctx, tenantID, apiVersion, req.PhoneNumberID, token); err != nil {
			h.log.Error("managed link: phone registration failed", zap.String("phone_number_id", req.PhoneNumberID), zap.Error(err))
			if d, ok := metaRefusalOf(err); !ok || d.Code != metaNeedsVerification {
				writeMetaRefusal(w, err, token, "Meta refused to register the number for Cloud API", "")
				return
			}
			verifyErr, pin = err, ""
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
	// is removed so nothing can pick it up later. The platform tenant keeps its own: that row can
	// be where the platform token itself is saved (LoadPlatformMetaCredentials merges it).
	if h.removeKeys != nil && !toPlatform {
		if err := h.removeKeys(ctx, tenantID, "access_token"); err != nil {
			h.log.Warn("managed link: could not remove the old tenant access token", zap.String("tenant_id", tenantID), zap.Error(err))
		}
	}
	pcfg.InvalidateCache()
	if verifyErr != nil {
		h.log.Info("whatsapp managed link saved, the number needs verification",
			zap.String("tenant_id", tenantID), zap.String("phone_number_id", req.PhoneNumberID))
		writeMetaRefusal(w, verifyErr, token, "The number is linked, but Meta refused to register it for Cloud API", "")
		return
	}

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

// graphGetJSON GETs a Graph path and decodes the answer into out. A refusal comes back as a
// *graphError carrying Meta's own error.
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
		return &graphError{status: resp.Status, detail: parseMetaError(body)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("Meta's answer could not be read")
	}
	return nil
}

// metaErrorDetail is the useful part of a Graph error body.
type metaErrorDetail struct {
	Message   string `json:"message"`
	UserTitle string `json:"error_user_title"`
	UserMsg   string `json:"error_user_msg"`
	Code      int    `json:"code"`
	Subcode   int    `json:"error_subcode"`
}

// graphError is a refusal Meta answered with (as opposed to Meta not being reached).
type graphError struct {
	status string
	detail metaErrorDetail
}

func (e *graphError) Error() string {
	if m := strings.TrimSpace(e.detail.Message); m != "" {
		return m
	}
	return e.status
}

// parseMetaError reads {"error":{...}} out of a Graph body; an unreadable body gives a zero value.
func parseMetaError(body []byte) metaErrorDetail {
	var e struct {
		Error metaErrorDetail `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error
}

// metaRefusalOf says whether Meta answered err with a refusal and, when it did, Meta's details.
// graphPost errors read "meta graph api <path>: <status>: <body>".
func metaRefusalOf(err error) (metaErrorDetail, bool) {
	var ge *graphError
	if errors.As(err, &ge) {
		return ge.detail, true
	}
	s := err.Error()
	if !strings.HasPrefix(s, "meta graph api ") {
		return metaErrorDetail{}, false
	}
	if i := strings.Index(s, "{"); i >= 0 {
		return parseMetaError([]byte(s[i:])), true
	}
	return metaErrorDetail{}, true
}

// metaHints are what to do about Meta refusals we have met, by error subcode.
var metaHints = map[int]string{
	2593005: "verify the number with a code sent by SMS or voice (Verify this number), then link again",
}

// metaCodeHints are the same, by error code, for refusals Meta sends without a subcode.
var metaCodeHints = map[int]string{
	133005: "two-step verification on this number has a different PIN: reset two-step verification for the number in WhatsApp Manager, then link again",
	133006: "Meta needs the number verified again: send a code by SMS or voice and enter it (Verify this number)",
	136024: "Meta could not check the code: send a new code and try again",
	136025: "the code is wrong or has expired: check it, or send a new code",
}

// metaNeedsVerification is Meta's 133006 "Phone number re-verification needed": the number has to
// be verified with an SMS or voice code before it can be registered.
const metaNeedsVerification = 133006

// sanitizeMetaText keeps Meta's text readable and safe to show: the access token (and any other
// secret passed, such as a verification code) is never echoed, control characters go and the
// length is capped.
func sanitizeMetaText(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

// writeMetaRefusal answers a failed Meta call. A refusal Meta answered is 422 with Meta's own
// title and message (error_user_title, error_user_msg) and a hint when one is known; the error
// text carries them too, so a client that shows only "error" still says what Meta said. A 133006
// also carries needs_verification, so the UI opens the verification step. Meta not answering is
// 502. hint is added when Meta has none of its own for the code. Every value in redact (the token,
// a verification code) is removed from what is shown.
func writeMetaRefusal(w http.ResponseWriter, err error, token, what, hint string, redact ...string) {
	if errors.Is(err, errNoPIN) {
		jsonError(w, http.StatusInternalServerError, errNoPIN.Error())
		return
	}
	secrets := append([]string{token}, redact...)
	d, answered := metaRefusalOf(err)
	if !answered {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": what + ": " + sanitizeMetaText(err.Error(), secrets...)})
		return
	}
	title := sanitizeMetaText(d.UserTitle, secrets...)
	msg := sanitizeMetaText(d.UserMsg, secrets...)
	if msg == "" {
		msg = sanitizeMetaText(d.Message, secrets...)
	}
	if msg == "" {
		msg = sanitizeMetaText(err.Error(), secrets...)
	}
	if h, ok := metaCodeHints[d.Code]; ok {
		hint = h
	}
	if h, ok := metaHints[d.Subcode]; ok {
		hint = h
	}
	text := what + ": "
	if title != "" {
		text += title + ". "
	}
	text += msg
	if hint != "" {
		text += "; " + hint
	}
	body := map[string]any{"error": text, "meta_message": msg}
	if title != "" {
		body["meta_title"] = title
	}
	if d.Code != 0 {
		body["meta_code"] = d.Code
	}
	if d.Subcode != 0 {
		body["meta_subcode"] = d.Subcode
	}
	if hint != "" {
		body["hint"] = hint
	}
	if d.Code == metaNeedsVerification || d.Subcode == 2593005 {
		body["needs_verification"] = true
	}
	jsonResponse(w, http.StatusUnprocessableEntity, body)
}
