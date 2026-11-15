package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	httpware "github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// In-app number verification (owner, 15 November 2026). A number deregistered for a while needs
// re-verification before Meta registers it again (133006 "Phone number re-verification needed").
// Instead of sending the owner to WhatsApp Manager, the settings page asks Meta for a code by SMS
// or voice, takes the code, verifies it and registers the number with its stored PIN.

// Verification rate limits, per tenant. Meta limits code requests too; ours stop a stuck button or
// a loop from burning through Meta's allowance and from guessing codes.
const (
	codeRequestGap      = 60 * time.Second // between two code requests
	codeRequestsPerHour = 5
	codeChecksPer15Min  = 10
)

// verificationRequest asks Meta for a verification code.
type verificationRequest struct {
	CodeMethod string `json:"code_method"` // SMS or VOICE
	Language   string `json:"language"`    // default en_US
}

// verificationCheck is the code the owner received.
type verificationCheck struct {
	Code string `json:"code"`
}

func (h *WhatsAppEmbeddedSignupHandler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// allowAttempt records an attempt under key when it is within the limits: at most max in window,
// and at least gap after the previous one. It returns how long to wait when it is not.
func (h *WhatsAppEmbeddedSignupHandler) allowAttempt(key string, max int, window, gap time.Duration) (bool, time.Duration) {
	h.verifyLimitsMu.Lock()
	defer h.verifyLimitsMu.Unlock()
	if h.verifyLimits == nil {
		h.verifyLimits = map[string][]time.Time{}
	}
	now := h.clock()
	kept := h.verifyLimits[key][:0]
	for _, t := range h.verifyLimits[key] {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	if n := len(kept); n > 0 && gap > 0 && now.Sub(kept[n-1]) < gap {
		h.verifyLimits[key] = kept
		return false, gap - now.Sub(kept[n-1])
	}
	if len(kept) >= max {
		h.verifyLimits[key] = kept
		return false, window - now.Sub(kept[0])
	}
	h.verifyLimits[key] = append(kept, now)
	return true, 0
}

func tooMany(w http.ResponseWriter, wait time.Duration, what string) {
	secs := int(wait.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	jsonResponse(w, http.StatusTooManyRequests, map[string]any{
		"error":               what + ": wait " + strconv.Itoa(secs) + " seconds and try again",
		"retry_after_seconds": secs,
	})
}

// verificationTarget resolves the platform owner's tenant and its linked number for the
// verification routes, answering the request itself when it cannot.
func (h *WhatsAppEmbeddedSignupHandler) verificationTarget(ctx context.Context, w http.ResponseWriter, r *http.Request) (tenantID, phoneID, token, apiVersion string, ok bool) {
	if !httpware.IsPlatformOwner(r.Context()) {
		jsonError(w, http.StatusForbidden, "only a platform owner can verify a number")
		return "", "", "", "", false
	}
	tenantID = h.actingOrPlatformTenant(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "choose the tenant first")
		return "", "", "", "", false
	}
	phoneID, token, version := h.linkedNumber(ctx, tenantID)
	if phoneID == "" {
		jsonError(w, http.StatusNotFound, "no WhatsApp number is linked for this tenant: link or connect it first")
		return "", "", "", "", false
	}
	if token == "" {
		jsonError(w, http.StatusServiceUnavailable, "no access token is available for this number: link it again from our portfolio, or reconnect it with Embedded Signup")
		return "", "", "", "", false
	}
	return tenantID, phoneID, token, whatsapp.GraphVersion(version), true
}

// RequestVerificationCode asks Meta to send the linked number a verification code by SMS or voice.
//
// @Summary Send a verification code to the linked WhatsApp number
// @Description Platform owners only, for the tenant in X-Tenant-ID or the platform tenant. Rate limited.
// @Tags WhatsApp
// @Accept json
// @Produce json
// @Param body body verificationRequest true "SMS or VOICE, and the language (default en_US)"
// @Success 200 {object} map[string]interface{}
// @Failure 400,403,404,422,429,502,503 {object} errorResponse
// @Security bearerAuth
// @Router /billing/whatsapp/verification/request [post]
func (h *WhatsAppEmbeddedSignupHandler) RequestVerificationCode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), signupTimeout)
	defer cancel()
	var req verificationRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	method := strings.ToUpper(strings.TrimSpace(req.CodeMethod))
	if method != "SMS" && method != "VOICE" {
		jsonError(w, http.StatusBadRequest, "code_method must be SMS or VOICE")
		return
	}
	language := strings.TrimSpace(req.Language)
	if language == "" {
		language = "en_US"
	}
	if !isLanguageCode(language) {
		jsonError(w, http.StatusBadRequest, "language must look like en_US")
		return
	}
	tenantID, phoneID, token, apiVersion, ok := h.verificationTarget(ctx, w, r)
	if !ok {
		return
	}
	if allowed, wait := h.allowAttempt("request:"+tenantID, codeRequestsPerHour, time.Hour, codeRequestGap); !allowed {
		tooMany(w, wait, "a code was sent a moment ago")
		return
	}
	if err := h.graphPost(ctx, apiVersion, phoneID+"/request_code", token, map[string]any{
		"code_method": method,
		"language":    language,
	}); err != nil {
		d, _ := metaRefusalOf(err)
		h.log.Warn("whatsapp verification: code request failed", zap.String("tenant_id", tenantID),
			zap.String("phone_number_id", phoneID), zap.Int("meta_code", d.Code), zap.Int("meta_subcode", d.Subcode))
		writeMetaRefusal(w, err, token, "Meta did not send the verification code", "")
		return
	}
	h.log.Info("whatsapp verification code requested", zap.String("tenant_id", tenantID), zap.String("phone_number_id", phoneID), zap.String("method", method))
	how := "by SMS"
	if method == "VOICE" {
		how = "by voice call"
	}
	jsonResponse(w, http.StatusOK, map[string]any{"message": "Verification code sent " + how, "code_method": method, "phone_number_id": phoneID})
}

// VerifyCode checks the code with Meta, then registers the number on Cloud API with its stored
// PIN (as Re-register does) and answers with the number's live state. The code is never logged
// or echoed.
//
// @Summary Verify the linked WhatsApp number with its code and register it
// @Tags WhatsApp
// @Accept json
// @Produce json
// @Param body body verificationCheck true "The code Meta sent"
// @Success 200 {object} map[string]interface{}
// @Failure 400,403,404,422,429,500,502,503 {object} errorResponse
// @Security bearerAuth
// @Router /billing/whatsapp/verification/verify [post]
func (h *WhatsAppEmbeddedSignupHandler) VerifyCode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), signupTimeout)
	defer cancel()
	var req verificationCheck
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	code := strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(req.Code))
	if len(code) < 4 || len(code) > 10 || !isGraphID(code) {
		jsonError(w, http.StatusBadRequest, "enter the code Meta sent (digits only)")
		return
	}
	tenantID, phoneID, token, apiVersion, ok := h.verificationTarget(ctx, w, r)
	if !ok {
		return
	}
	if allowed, wait := h.allowAttempt("verify:"+tenantID, codeChecksPer15Min, 15*time.Minute, 0); !allowed {
		tooMany(w, wait, "too many code checks")
		return
	}
	if err := h.graphPost(ctx, apiVersion, phoneID+"/verify_code", token, map[string]any{"code": code}); err != nil {
		d, _ := metaRefusalOf(err)
		h.log.Warn("whatsapp verification: code refused", zap.String("tenant_id", tenantID),
			zap.String("phone_number_id", phoneID), zap.Int("meta_code", d.Code), zap.Int("meta_subcode", d.Subcode))
		writeMetaRefusal(w, err, token, "Meta did not accept the code", "", code)
		return
	}
	h.log.Info("whatsapp number verified", zap.String("tenant_id", tenantID), zap.String("phone_number_id", phoneID))

	pin, err := h.registerNumber(ctx, tenantID, apiVersion, phoneID, token)
	if err != nil {
		h.log.Error("whatsapp verification: registration after verify failed", zap.String("tenant_id", tenantID), zap.String("phone_number_id", phoneID), zap.Error(err))
		writeMetaRefusal(w, err, token, "the number was verified, but Meta refused to register it for Cloud API", "", code)
		return
	}
	encPIN, err := h.encrypt(pin)
	if err == nil {
		err = h.save(ctx, tenantID, []signupSetting{{key: "registration_pin", value: encPIN, secret: true}})
	}
	if err != nil {
		h.log.Error("whatsapp verification: could not save the PIN", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "the number was verified and registered but its PIN could not be saved, contact support")
		return
	}
	pcfg.InvalidateCache()

	out := map[string]any{"message": "WhatsApp number verified and registered", "phone_number_id": phoneID, "registered": true}
	var phone struct {
		PlatformType           string `json:"platform_type"`
		Status                 string `json:"status"`
		CodeVerificationStatus string `json:"code_verification_status"`
	}
	if err := h.graphGetJSON(ctx, apiVersion, phoneID+"?fields=platform_type,status,code_verification_status", token, &phone); err == nil {
		out["status"] = phone.Status
		out["platform_type"] = phone.PlatformType
		out["code_verification_status"] = phone.CodeVerificationStatus
		out["registered"] = registeredOnCloudAPI(phone.PlatformType, phone.Status)
	}
	jsonResponse(w, http.StatusOK, out)
}

// isLanguageCode accepts Meta's language codes such as en_US or sw.
func isLanguageCode(s string) bool {
	if len(s) < 2 || len(s) > 10 {
		return false
	}
	for _, c := range s {
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}
