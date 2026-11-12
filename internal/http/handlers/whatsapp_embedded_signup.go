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
	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// defaultGraphBase is Meta's Graph API host; tests point the handler at a fake one.
const defaultGraphBase = "https://graph.facebook.com"

// signupTimeout bounds the whole completion (code exchange, subscribe, status check, register,
// save); graphCallTimeout bounds each call to Meta. Register alone has taken over 14 seconds.
const (
	signupTimeout    = 3 * time.Minute
	graphCallTimeout = 90 * time.Second
)

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
	// autoSync, when set, submits the templates the tenant uses to its new WABA (owner decision
	// "Auto-sync on connect + button"). It runs in the background after a successful completion
	// only, on a context detached from the request; its outcome is logged and never changes the
	// signup response.
	autoSync func(ctx context.Context, tenantID string)
}

// WithTemplateAutoSync sets the background template sync started after a successful signup.
func (h *WhatsAppEmbeddedSignupHandler) WithTemplateAutoSync(fn func(ctx context.Context, tenantID string)) *WhatsAppEmbeddedSignupHandler {
	h.autoSync = fn
	return h
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
		http:      &http.Client{Timeout: graphCallTimeout},
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
	// The calls to Meta run to the end even if the browser gives up first: Meta's register call
	// can take longer than the UI's request timeout, and cancelling it halfway left a number
	// half registered with a PIN nobody kept (2026-11-12).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), signupTimeout)
	defer cancel()
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
	apiVersion := whatsapp.GraphVersion(creds["api_version"])

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

	// A number already on Cloud API (an earlier attempt registered it) is not registered again:
	// a new PIN would be refused against the one that attempt set.
	pin := ""
	if !h.onCloudAPI(ctx, apiVersion, req.PhoneNumberID, businessToken) {
		if pin, err = h.newPIN(); err != nil {
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
	}

	rows := []signupSetting{
		{key: "phone_number_id", value: req.PhoneNumberID},
		{key: "waba_id", value: req.WABAID},
	}
	encToken, err := h.encrypt(businessToken)
	if err == nil {
		rows = append(rows, signupSetting{key: "access_token", value: encToken, secret: true})
		if pin != "" {
			var encPIN string
			if encPIN, err = h.encrypt(pin); err == nil {
				rows = append(rows, signupSetting{key: "registration_pin", value: encPIN, secret: true})
			}
		}
	}
	if err == nil {
		err = h.save(ctx, tenantID, rows)
	}
	if err != nil {
		h.log.Error("embedded signup: failed to store tenant settings", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "connected to Meta but failed to save the number, contact support")
		return
	}

	// Sends and the template sync read these rows through a short per-pod cache; drop it so the
	// new number and token are used at once rather than after the cache expires.
	pcfg.InvalidateCache()

	h.log.Info("whatsapp embedded signup completed",
		zap.String("tenant_id", tenantID), zap.String("waba_id", req.WABAID), zap.String("phone_number_id", req.PhoneNumberID))
	if h.autoSync != nil {
		// Detached from the request, which ends with this response. The caller's claims stay in
		// the context, so its active services can steer the template selection.
		go h.autoSync(context.WithoutCancel(r.Context()), tenantID)
	}
	resp := map[string]any{
		"message":         "WhatsApp number connected",
		"phone_number_id": req.PhoneNumberID,
	}
	// Meta refuses paid template messages (#131042) from an account with no payment method, and
	// there is no API to add a card, so say so now instead of at the first failed send (owner,
	// 2026-11-12: Codevertex attaches its card once per tenant account).
	if has, known := h.hasPaymentMethod(ctx, apiVersion, req.WABAID, businessToken); known && !has {
		h.log.Warn("embedded signup: the new WhatsApp account has no payment method",
			zap.String("tenant_id", tenantID), zap.String("waba_id", req.WABAID))
		resp["payment_method_missing"] = true
		resp["waba_id"] = req.WABAID
	}
	jsonResponse(w, http.StatusOK, resp)
}

// hasPaymentMethod reads the account's primary_funding_id. known is false when Meta could not be
// asked, so a failed check never claims a payment method is missing.
func (h *WhatsAppEmbeddedSignupHandler) hasPaymentMethod(ctx context.Context, apiVersion, wabaID, accessToken string) (has, known bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/%s/%s?fields=primary_funding_id", h.graphBase, apiVersion, wabaID), nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := h.http.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return false, false
	}
	var out struct {
		PrimaryFundingID string `json:"primary_funding_id"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return false, false
	}
	return out.PrimaryFundingID != "", true
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

// onCloudAPI reports whether Meta already lists the number as registered on Cloud API. Any
// failure to tell counts as not registered, so register is attempted as before.
func (h *WhatsAppEmbeddedSignupHandler) onCloudAPI(ctx context.Context, apiVersion, phoneNumberID, accessToken string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/%s/%s?fields=platform_type,status", h.graphBase, apiVersion, phoneNumberID), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := h.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return false
	}
	var out struct {
		PlatformType string `json:"platform_type"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return false
	}
	return out.PlatformType == "CLOUD_API"
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
