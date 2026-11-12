package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	"github.com/bengobox/notifications-api/internal/providers"
)

// defaultGraphBase is Meta's Graph API host; tests point the handler at a fake one.
const defaultGraphBase = "https://graph.facebook.com"

// WhatsAppEmbeddedSignupHandler completes a tenant's WhatsApp Embedded Signup, Meta's flow for a
// Tech Provider (Codevertex: one app, many businesses, each with its own number):
// https://developers.facebook.com/docs/whatsapp/embedded-signup/implementation
//
// The popup returns a one-time code plus the new WABA and phone number IDs. The code is exchanged
// for a business integration token, and that token (never the platform's own token, which has no
// rights on a WABA the customer just created) subscribes our webhook, registers the number and is
// stored as the tenant's own encrypted access_token for its sends. Before this, the platform token
// was used and Meta answered "Object with ID ... cannot be loaded due to missing permissions"
// (2026-11-12).
//
// Credit-line sharing is still a manual step in Business Manager.
type WhatsAppEmbeddedSignupHandler struct {
	client    *ent.Client
	log       *zap.Logger
	http      *http.Client
	graphBase string
	creds     func(context.Context) (map[string]string, error)
	encrypt   func(string) (string, error)
	newPIN    func() (string, error)
	save      func(ctx context.Context, tenantID string, rows []signupSetting) error
}

// signupSetting is one tenant provider_settings row written on completion.
type signupSetting struct {
	key    string
	value  string
	secret bool
}

func NewWhatsAppEmbeddedSignupHandler(client *ent.Client, log *zap.Logger, pm *providers.Manager) *WhatsAppEmbeddedSignupHandler {
	h := &WhatsAppEmbeddedSignupHandler{
		client:    client,
		log:       log.Named("whatsapp.embedded_signup"),
		http:      &http.Client{Timeout: 15 * time.Second},
		graphBase: defaultGraphBase,
		newPIN:    randomPIN,
	}
	h.creds = func(ctx context.Context) (map[string]string, error) {
		s, err := pm.LoadPlatformMetaCredentials(ctx)
		return map[string]string(s), err
	}
	h.encrypt = pm.EncryptSecret
	h.save = h.saveTenantSettings
	return h
}

type completeSignupRequest struct {
	Code          string `json:"code"`
	WABAID        string `json:"waba_id"`
	PhoneNumberID string `json:"phone_number_id"`
}

// Complete finalizes a tenant's Embedded Signup and saves the number as the tenant's own WhatsApp
// settings (the same rows the manual settings form writes, so sends resolve to it unchanged).
func (h *WhatsAppEmbeddedSignupHandler) Complete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := resolveActingTenantID(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}

	var req completeSignupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" || req.WABAID == "" || req.PhoneNumberID == "" {
		jsonError(w, http.StatusBadRequest, "code, waba_id and phone_number_id are required")
		return
	}

	creds, err := h.creds(ctx)
	if err != nil {
		h.log.Error("embedded signup: could not load platform Meta settings", zap.Error(err))
	}
	appID := firstNonEmptyValue(creds["app_id"], os.Getenv("WHATSAPP_APP_ID"))
	appSecret := firstNonEmptyValue(creds["app_secret"], os.Getenv("WHATSAPP_APP_SECRET"))
	if appID == "" || appSecret == "" {
		jsonError(w, http.StatusServiceUnavailable,
			"WhatsApp App ID and App Secret are not set: add them under Platform > Providers > WhatsApp (Meta Cloud)")
		return
	}
	apiVersion := firstNonEmptyValue(creds["api_version"], "v21.0")

	businessToken, err := h.exchangeCode(ctx, apiVersion, appID, appSecret, req.Code)
	if err != nil {
		h.log.Error("embedded signup: code exchange failed", zap.String("waba_id", req.WABAID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, "Meta did not accept the signup code, please start the connection again")
		return
	}

	if err := h.graphPost(ctx, apiVersion, req.WABAID+"/subscribed_apps", businessToken, nil); err != nil {
		h.log.Error("embedded signup: webhook subscription failed", zap.String("waba_id", req.WABAID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, "failed to subscribe webhook to the new WhatsApp Business Account: "+err.Error())
		return
	}

	pin, err := h.newPIN()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "could not create a registration PIN")
		return
	}
	if err := h.graphPost(ctx, apiVersion, req.PhoneNumberID+"/register", businessToken, map[string]any{
		"messaging_product": "whatsapp",
		"pin":               pin,
	}); err != nil {
		h.log.Error("embedded signup: phone registration failed", zap.String("phone_number_id", req.PhoneNumberID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, "failed to register the phone number for Cloud API: "+err.Error())
		return
	}

	encToken, err := h.encrypt(businessToken)
	if err == nil {
		var encPIN string
		if encPIN, err = h.encrypt(pin); err == nil {
			err = h.save(ctx, tenantID, []signupSetting{
				{key: "phone_number_id", value: req.PhoneNumberID},
				{key: "waba_id", value: req.WABAID},
				{key: "access_token", value: encToken, secret: true},
				{key: "registration_pin", value: encPIN, secret: true},
			})
		}
	}
	if err != nil {
		h.log.Error("embedded signup: failed to store tenant settings", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "connected to Meta but failed to save the number, contact support")
		return
	}

	h.log.Info("whatsapp embedded signup completed",
		zap.String("tenant_id", tenantID), zap.String("waba_id", req.WABAID), zap.String("phone_number_id", req.PhoneNumberID))
	jsonResponse(w, http.StatusOK, map[string]any{
		"message":         "WhatsApp number connected",
		"phone_number_id": req.PhoneNumberID,
	})
}

// exchangeCode trades the popup's one-time code for the business integration token. Errors never
// carry the request URL, which holds the app secret.
func (h *WhatsAppEmbeddedSignupHandler) exchangeCode(ctx context.Context, apiVersion, appID, appSecret, code string) (string, error) {
	q := url.Values{"client_id": {appID}, "client_secret": {appSecret}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/%s/oauth/access_token?%s", h.graphBase, apiVersion, q.Encode()), nil)
	if err != nil {
		return "", errors.New("build code exchange request")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return "", errors.New("code exchange request failed")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("code exchange: %s: %s", resp.Status, string(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", errors.New("code exchange: no access_token in the answer")
	}
	return out.AccessToken, nil
}

func (h *WhatsAppEmbeddedSignupHandler) graphPost(ctx context.Context, apiVersion, path, accessToken string, body map[string]any) error {
	payload := []byte("{}")
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/%s", h.graphBase, apiVersion, path), bytes.NewReader(payload))
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

// saveTenantSettings upserts each row under the tenant's whatsapp/meta_cloud production settings.
func (h *WhatsAppEmbeddedSignupHandler) saveTenantSettings(ctx context.Context, tenantID string, rows []signupSetting) error {
	for _, row := range rows {
		existing, err := h.client.ProviderSetting.Query().
			Where(
				providersetting.TenantID(tenantID),
				providersetting.Environment("production"),
				providersetting.ProviderType("whatsapp"),
				providersetting.ProviderName("meta_cloud"),
				providersetting.Key(row.key),
			).
			First(ctx)
		if err == nil {
			if _, err := existing.Update().SetValue(row.value).
				SetIsSecret(row.secret).SetIsEncrypted(row.secret).
				SetIsActive(true).SetStatus("active").Save(ctx); err != nil {
				return err
			}
			continue
		}
		if !ent.IsNotFound(err) {
			return err
		}
		if err := h.client.ProviderSetting.Create().
			SetTenantID(tenantID).
			SetChannel("whatsapp").
			SetProvider("meta_cloud").
			SetProviderType("whatsapp").
			SetProviderName("meta_cloud").
			SetEnvironment("production").
			SetKey(row.key).
			SetValue(row.value).
			SetIsSecret(row.secret).
			SetIsEncrypted(row.secret).
			SetIsActive(true).
			SetStatus("active").
			Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// randomPIN is the six-digit two-step verification PIN Meta requires to register a number.
func randomPIN() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func firstNonEmptyValue(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
