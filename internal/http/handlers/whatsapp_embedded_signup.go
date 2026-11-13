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
	// profileSync, when set, fills the new number's business profile (about, email, website,
	// photo) from the tenant's branding, so customers do not see a bare number (2026-11-12). Same
	// rules as autoSync: background, after success only, never changes the response.
	profileSync func(ctx context.Context, tenantID string)
	// conn reads the tenant's own connection (never the platform's) for Disconnect: phone number id,
	// WABA id, business token and Graph version; empty values when the tenant has none.
	conn func(ctx context.Context, tenantID string) (phoneID, wabaID, token, version string)
	// remove deletes the tenant's own whatsapp/meta_cloud connection rows.
	remove func(ctx context.Context, tenantID string) error
	// removeKeys deletes the named tenant whatsapp/meta_cloud rows: a managed link drops an old
	// tenant access_token, and Embedded Signup drops an old token_source marker.
	removeKeys func(ctx context.Context, tenantID string, keys ...string) error
	// linkedElsewhere reports whether the phone number ID or WhatsApp account ID is already saved
	// on a tenant other than tenantID.
	linkedElsewhere func(ctx context.Context, tenantID, phoneNumberID, wabaID string) (bool, error)
	// platformID is the platform tenant, whose own number is never linked to another tenant.
	platformID string
}

// connectionKeys are the tenant rows a connection writes and Disconnect removes. Other settings
// (for example the preferred provider) stay.
var connectionKeys = []string{"phone_number_id", "waba_id", "access_token", "registration_pin", providers.TokenSourceKey}

// WithTemplateAutoSync sets the background template sync started after a successful signup.
func (h *WhatsAppEmbeddedSignupHandler) WithTemplateAutoSync(fn func(ctx context.Context, tenantID string)) *WhatsAppEmbeddedSignupHandler {
	h.autoSync = fn
	return h
}

// WithProfileAutoSync sets the background business profile fill started after a successful signup.
func (h *WhatsAppEmbeddedSignupHandler) WithProfileAutoSync(fn func(ctx context.Context, tenantID string)) *WhatsAppEmbeddedSignupHandler {
	h.profileSync = fn
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
	h.conn = func(ctx context.Context, tenantID string) (string, string, string, string) {
		phoneID, token, version, _ := pm.LoadTenantWhatsAppProfileCredentials(ctx, tenantID)
		wabaID, wabaToken, _ := pm.LoadTenantWhatsAppTemplateCredentials(ctx, tenantID)
		if token == "" {
			token = wabaToken
		}
		return phoneID, wabaID, token, version
	}
	h.remove = h.removeTenantConnection
	h.removeKeys = h.removeTenantKeys
	h.linkedElsewhere = h.numberLinkedElsewhere
	h.platformID = pm.PlatformID
	return h
}

// disconnectResult says what was undone on Meta; local removal always happens.
type disconnectResult struct {
	Message string `json:"message"`
	// Deregistered is true when Meta released the number from Cloud API.
	Deregistered bool `json:"deregistered"`
	// Unsubscribed is true when our app stopped receiving the account's webhooks.
	Unsubscribed bool `json:"unsubscribed"`
	// Notes explain any Meta step that could not be done, e.g. an account already deleted.
	Notes []string `json:"notes,omitempty"`
}

// Disconnect removes the tenant's own WhatsApp connection (owner, 2026-11-12). On Meta it
// deregisters the number from Cloud API and unsubscribes our app from the account, each best
// effort: an account the owner already deleted in WhatsApp Manager only adds a note. Then it
// deletes the tenant's connection rows (number, account, token, PIN), so sends stop at once and a
// fresh Embedded Signup starts clean. Meta has no API to delete a WhatsApp Business Account; the
// note says to remove it in WhatsApp Manager when wanted.
//
// @Summary Disconnect the tenant's WhatsApp number
// @Description Deregisters the number and unsubscribes the app on Meta (best effort), then removes the tenant's saved connection.
// @Tags WhatsApp
// @Produce json
// @Success 200 {object} disconnectResult
// @Failure 400,404,500 {object} errorResponse
// @Security bearerAuth
// @Router /billing/whatsapp/connection [delete]
func (h *WhatsAppEmbeddedSignupHandler) Disconnect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), signupTimeout)
	defer cancel()
	tenantID := resolveActingTenantID(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}
	phoneID, wabaID, token, version := h.conn(ctx, tenantID)
	if phoneID == "" && wabaID == "" {
		jsonError(w, http.StatusNotFound, "no WhatsApp number is connected")
		return
	}
	res := disconnectResult{Message: "WhatsApp number disconnected"}
	apiVersion := whatsapp.GraphVersion(version)
	if token == "" {
		res.Notes = append(res.Notes, "no stored access token, so nothing was changed on Meta")
	} else {
		if phoneID != "" {
			if err := h.graphPost(ctx, apiVersion, phoneID+"/deregister", token, nil); err != nil {
				h.log.Warn("whatsapp disconnect: deregister failed", zap.String("tenant_id", tenantID), zap.Error(err))
				res.Notes = append(res.Notes, "the number could not be deregistered on Meta (it may already be deleted)")
			} else {
				res.Deregistered = true
			}
		}
		if wabaID != "" {
			if err := h.graphDelete(ctx, apiVersion, wabaID+"/subscribed_apps", token); err != nil {
				h.log.Warn("whatsapp disconnect: unsubscribe failed", zap.String("tenant_id", tenantID), zap.Error(err))
				res.Notes = append(res.Notes, "the WhatsApp account could not be unsubscribed on Meta (it may already be deleted)")
			} else {
				res.Unsubscribed = true
			}
		}
	}
	if err := h.remove(ctx, tenantID); err != nil {
		h.log.Error("whatsapp disconnect: removing local connection failed", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "could not remove the saved WhatsApp connection, try again")
		return
	}
	pcfg.InvalidateCache()
	res.Notes = append(res.Notes, "to delete the WhatsApp account itself, remove it in WhatsApp Manager; Meta offers no API for that")
	h.log.Info("whatsapp number disconnected", zap.String("tenant_id", tenantID),
		zap.String("waba_id", wabaID), zap.String("phone_number_id", phoneID),
		zap.Bool("deregistered", res.Deregistered), zap.Bool("unsubscribed", res.Unsubscribed))
	jsonResponse(w, http.StatusOK, res)
}

func (h *WhatsAppEmbeddedSignupHandler) graphDelete(ctx context.Context, apiVersion, path, accessToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/%s/%s", h.graphBase, apiVersion, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("meta graph api %s: %s: %s", path, resp.Status, string(body))
	}
	return nil
}

// removeTenantConnection deletes the tenant's own connection rows (every environment).
func (h *WhatsAppEmbeddedSignupHandler) removeTenantConnection(ctx context.Context, tenantID string) error {
	return h.removeTenantKeys(ctx, tenantID, connectionKeys...)
}

// removeTenantKeys deletes the named tenant whatsapp/meta_cloud rows (every environment).
func (h *WhatsAppEmbeddedSignupHandler) removeTenantKeys(ctx context.Context, tenantID string, keys ...string) error {
	_, err := h.client.ProviderSetting.Delete().
		Where(
			providersetting.TenantID(tenantID),
			providersetting.ProviderType("whatsapp"),
			providersetting.ProviderName("meta_cloud"),
			providersetting.KeyIn(keys...),
		).
		Exec(ctx)
	return err
}

// numberLinkedElsewhere looks for the phone number ID or WhatsApp account ID on any other tenant's
// whatsapp/meta_cloud rows (both are saved in plain text).
func (h *WhatsAppEmbeddedSignupHandler) numberLinkedElsewhere(ctx context.Context, tenantID, phoneNumberID, wabaID string) (bool, error) {
	return h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDNEQ(tenantID),
			providersetting.ProviderType("whatsapp"),
			providersetting.ProviderName("meta_cloud"),
			providersetting.Or(
				providersetting.And(providersetting.Key("phone_number_id"), providersetting.Value(phoneNumberID)),
				providersetting.And(providersetting.Key("waba_id"), providersetting.Value(wabaID)),
			),
		).
		Exist(ctx)
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
	// A tenant moving from a managed link to its own account must use its new business token, so
	// the managed link marker goes.
	if h.removeKeys != nil {
		if err := h.removeKeys(ctx, tenantID, providers.TokenSourceKey); err != nil {
			h.log.Error("embedded signup: could not remove the managed link marker", zap.String("tenant_id", tenantID), zap.Error(err))
			jsonError(w, http.StatusInternalServerError, "connected to Meta but failed to save the number, contact support")
			return
		}
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
	if h.profileSync != nil {
		go h.profileSync(context.WithoutCancel(r.Context()), tenantID)
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
