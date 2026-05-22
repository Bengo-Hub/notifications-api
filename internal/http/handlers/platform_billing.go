package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type PlatformBilling struct {
	client *ent.Client
	logger *zap.Logger
}

func NewPlatformBilling(client *ent.Client, logger *zap.Logger) *PlatformBilling {
	return &PlatformBilling{
		client: client,
		logger: logger.Named("platform_billing"),
	}
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
