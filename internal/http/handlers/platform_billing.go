package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/modules/billing"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type PlatformBilling struct {
	client       *ent.Client
	logger       *zap.Logger
	billingSvc   *billing.Service
	whatsappSubs *billing.WhatsAppSubscriptionService
}

func NewPlatformBilling(client *ent.Client, logger *zap.Logger, billingSvc *billing.Service, whatsappSubs *billing.WhatsAppSubscriptionService) *PlatformBilling {
	return &PlatformBilling{
		client:       client,
		logger:       logger.Named("platform_billing"),
		billingSvc:   billingSvc,
		whatsappSubs: whatsappSubs,
	}
}

// marginResponse combines realized SMS and WhatsApp margin for the platform admin billing view —
// what tenants are actually charged vs. the platform's real provider cost, captured per-transaction
// (SMS) or per-active-subscription (WhatsApp) but never surfaced anywhere until now.
type marginResponse struct {
	SMS      billing.SMSMarginSummary      `json:"sms"`
	WhatsApp billing.WhatsAppMarginSummary `json:"whatsapp"`
}

// GetMargin returns realized SMS + WhatsApp margin, optionally bounded by ?from=&to= (RFC3339;
// SMS only — WhatsApp margin is always a live snapshot of active subscriptions, not historical).
// GET /platform/billing/margin
func (h *PlatformBilling) GetMargin(w http.ResponseWriter, r *http.Request) {
	var from, to *time.Time
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = &t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = &t
		}
	}

	resp := marginResponse{}
	if h.billingSvc != nil {
		sms, err := h.billingSvc.GetSMSMargin(r.Context(), nil, from, to)
		if err != nil {
			h.logger.Error("failed to compute sms margin", zap.Error(err))
			jsonError(w, http.StatusInternalServerError, "failed to compute sms margin")
			return
		}
		resp.SMS = sms
	}
	if h.whatsappSubs != nil {
		wa, err := h.whatsappSubs.GetMargin(r.Context())
		if err != nil {
			h.logger.Error("failed to compute whatsapp margin", zap.Error(err))
			jsonError(w, http.StatusInternalServerError, "failed to compute whatsapp margin")
			return
		}
		resp.WhatsApp = wa
	}

	jsonResponse(w, http.StatusOK, resp)
}

type updateBillingRequest struct {
	CostPerSMS              float64    `json:"cost_per_sms"`
	CostPerWhatsApp         float64    `json:"cost_per_whatsapp"`
	ProviderCostPerSMS      float64    `json:"provider_cost_per_sms"`
	ProviderCostPerWhatsApp float64    `json:"provider_cost_per_whatsapp"`
	MinMarkupPercentage     float64    `json:"min_markup_percentage"`
	MinTopupAmount          float64    `json:"min_topup_amount"`
	TreasuryGatewayID       *uuid.UUID `json:"treasury_gateway_id,omitempty"`
}

func (req *updateBillingRequest) validate() error {
	if req.ProviderCostPerSMS > 0 && req.CostPerSMS > 0 {
		minRate := req.ProviderCostPerSMS * (1 + req.MinMarkupPercentage/100)
		if req.CostPerSMS < minRate {
			return fmt.Errorf("cost_per_sms %.4f is below minimum %.4f (provider_cost %.4f × markup %.0f%%)",
				req.CostPerSMS, minRate, req.ProviderCostPerSMS, req.MinMarkupPercentage)
		}
	}
	return nil
}

func (h *PlatformBilling) GetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.client.PlatformBilling.Query().First(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonResponse(w, http.StatusOK, map[string]interface{}{})
			return
		}
		h.logger.Error("failed to get billing settings", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to get settings")
		return
	}
	jsonResponse(w, http.StatusOK, settings)
}

func (h *PlatformBilling) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req updateBillingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}

	if err := req.validate(); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	settings, err := h.client.PlatformBilling.Query().First(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			builder := h.client.PlatformBilling.Create().
				SetCostPerSms(req.CostPerSMS).
				SetCostPerWhatsapp(req.CostPerWhatsApp).
				SetMinTopupAmount(req.MinTopupAmount)
			if req.ProviderCostPerSMS > 0 {
				builder.SetProviderCostPerSms(req.ProviderCostPerSMS)
			}
			if req.ProviderCostPerWhatsApp > 0 {
				builder.SetProviderCostPerWhatsapp(req.ProviderCostPerWhatsApp)
			}
			if req.MinMarkupPercentage > 0 {
				builder.SetMinMarkupPercentage(req.MinMarkupPercentage)
			}
			if req.TreasuryGatewayID != nil {
				builder.SetTreasuryGatewayID(*req.TreasuryGatewayID)
			}
			settings, err = builder.Save(r.Context())
		} else {
			h.logger.Error("failed to query billing settings", zap.Error(err))
			jsonError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	} else {
		builder := settings.Update().
			SetCostPerSms(req.CostPerSMS).
			SetCostPerWhatsapp(req.CostPerWhatsApp).
			SetMinTopupAmount(req.MinTopupAmount)
		if req.ProviderCostPerSMS > 0 {
			builder.SetProviderCostPerSms(req.ProviderCostPerSMS)
		}
		if req.ProviderCostPerWhatsApp > 0 {
			builder.SetProviderCostPerWhatsapp(req.ProviderCostPerWhatsApp)
		}
		if req.MinMarkupPercentage > 0 {
			builder.SetMinMarkupPercentage(req.MinMarkupPercentage)
		}
		if req.TreasuryGatewayID != nil {
			builder.SetTreasuryGatewayID(*req.TreasuryGatewayID)
		}
		settings, err = builder.Save(r.Context())
	}

	if err != nil {
		h.logger.Error("failed to save billing settings", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to save settings")
		return
	}

	jsonResponse(w, http.StatusOK, settings)
}
