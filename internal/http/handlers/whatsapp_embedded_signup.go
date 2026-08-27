package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	"github.com/bengobox/notifications-api/internal/providers"
)

// WhatsAppEmbeddedSignupHandler completes a tenant's WhatsApp Embedded Signup flow — the Meta-
// recommended way for a Solution/Tech Provider (which is what CodeVertex is here: one app, many
// tenant businesses each with their own WhatsApp number) to onboard a client's number, per
// https://developers.facebook.com/documentation/business-messaging/whatsapp/solution-providers/support/migrating-phone-numbers-among-solution-partners-via-embedded-signup.
//
// This is the BACKEND HALF ONLY. The frontend half — the actual Embedded Signup popup (Meta's JS
// SDK, FB.login() with a config_id from App Dashboard → WhatsApp → Embedded Signup → Configurations)
// — is NOT built yet: that config_id doesn't exist until the user's Meta App has Embedded Signup
// configured (needs business verification + App Review for whatsapp_business_management /
// whatsapp_business_messaging at Advanced Access — a Meta-side prerequisite, not something buildable
// from here). Built ahead of that so the completion endpoint is ready the moment a real config_id
// exists to wire a frontend button to. NOT yet live-tested end-to-end for the same reason.
type WhatsAppEmbeddedSignupHandler struct {
	client *ent.Client
	log    *zap.Logger
	pm     *providers.Manager
	http   *http.Client
}

func NewWhatsAppEmbeddedSignupHandler(client *ent.Client, log *zap.Logger, pm *providers.Manager) *WhatsAppEmbeddedSignupHandler {
	return &WhatsAppEmbeddedSignupHandler{
		client: client, log: log.Named("whatsapp.embedded_signup"), pm: pm,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

type completeSignupRequest struct {
	WABAID        string `json:"waba_id"`
	PhoneNumberID string `json:"phone_number_id"`
}

// Complete finalizes a tenant's Embedded Signup: subscribes our webhook to the new WABA, registers
// the phone number for Cloud API use, and persists the phone_number_id as this tenant's own
// provider_settings override (the same field the manual settings-page form writes, so both paths
// converge on one place — real sends for this tenant then correctly resolve to their own number
// via the existing tenant-override-over-platform-fallback hierarchy, no further wiring needed).
//
// Deliberately does NOT attempt credit-line sharing — Meta's docs describe this step ("share our
// credit line with the new WABA") without a single, unambiguous API endpoint name confirmed here;
// shipping a guessed call for a real-money billing relationship is worse than leaving it as a
// manual step (Meta Business Manager → the new WABA → billing) until the exact call is verified
// against Meta's current Graph API reference.
func (h *WhatsAppEmbeddedSignupHandler) Complete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := resolveActingTenantID(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}

	var req completeSignupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.WABAID == "" || req.PhoneNumberID == "" {
		jsonError(w, http.StatusBadRequest, "waba_id and phone_number_id are required")
		return
	}

	// Our own platform-level Meta credentials act on the new WABA — Embedded Signup automatically
	// grants OUR app access to it the moment the client completes the flow, so we never need (and
	// never see) the client's own Meta user token.
	platformSettings, err := h.pm.LoadPlatformMetaCredentials(ctx)
	if err != nil || platformSettings["access_token"] == "" {
		h.log.Error("embedded signup: could not resolve platform Meta credentials", zap.Error(err))
		jsonError(w, http.StatusServiceUnavailable, "platform WhatsApp credentials not configured")
		return
	}
	accessToken := platformSettings["access_token"]
	apiVersion := platformSettings["api_version"]
	if apiVersion == "" {
		apiVersion = "v21.0"
	}

	if err := h.graphPost(ctx, apiVersion, req.WABAID+"/subscribed_apps", accessToken, nil); err != nil {
		h.log.Error("embedded signup: webhook subscription failed", zap.String("waba_id", req.WABAID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, "failed to subscribe webhook to the new WhatsApp Business Account: "+err.Error())
		return
	}

	if err := h.graphPost(ctx, apiVersion, req.PhoneNumberID+"/register", accessToken, map[string]any{
		"messaging_product": "whatsapp",
	}); err != nil {
		h.log.Error("embedded signup: phone registration failed", zap.String("phone_number_id", req.PhoneNumberID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, "failed to register the phone number for Cloud API: "+err.Error())
		return
	}

	if err := h.saveTenantPhoneNumber(ctx, tenantID, req.PhoneNumberID); err != nil {
		h.log.Error("embedded signup: failed to persist tenant phone number", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "connected to Meta but failed to save the number — contact support")
		return
	}

	h.log.Info("whatsapp embedded signup completed",
		zap.String("tenant_id", tenantID), zap.String("waba_id", req.WABAID), zap.String("phone_number_id", req.PhoneNumberID))
	jsonResponse(w, http.StatusOK, map[string]any{
		"message":         "WhatsApp number connected",
		"phone_number_id": req.PhoneNumberID,
	})
}

func (h *WhatsAppEmbeddedSignupHandler) graphPost(ctx context.Context, apiVersion, path, accessToken string, body map[string]any) error {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://graph.facebook.com/%s/%s", apiVersion, path), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("meta graph api %s: %s: %s", path, resp.Status, string(respBody))
	}
	return nil
}

func (h *WhatsAppEmbeddedSignupHandler) saveTenantPhoneNumber(ctx context.Context, tenantID, phoneNumberID string) error {
	existing, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantID(tenantID),
			providersetting.Environment("production"),
			providersetting.ProviderType("whatsapp"),
			providersetting.ProviderName("meta_cloud"),
			providersetting.Key("phone_number_id"),
		).
		First(ctx)
	if err == nil {
		_, err = existing.Update().SetValue(phoneNumberID).Save(ctx)
		return err
	}
	if !ent.IsNotFound(err) {
		return err
	}
	return h.client.ProviderSetting.Create().
		SetTenantID(tenantID).
		SetChannel("whatsapp").
		SetProvider("meta_cloud").
		SetProviderType("whatsapp").
		SetProviderName("meta_cloud").
		SetEnvironment("production").
		SetKey("phone_number_id").
		SetValue(phoneNumberID).
		SetIsActive(true).
		SetStatus("active").
		Exec(ctx)
}
