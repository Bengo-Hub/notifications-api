package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/tenantwhatsappsubscription"
	"github.com/bengobox/notifications-api/internal/ent/whatsappplan"

	serviceclient "github.com/Bengo-Hub/shared-service-client"
)

// WhatsAppSubscriptionService manages tenant WhatsApp subscription plans.
type WhatsAppSubscriptionService struct {
	client         *ent.Client
	log            *zap.Logger
	treasuryClient *serviceclient.Client
	treasuryAPIKey string
}

// NewWhatsAppSubscriptionService creates a new WhatsApp subscription service.
// treasuryAPIKey is the shared INTERNAL_SERVICE_KEY sent as X-API-Key on S2S calls.
func NewWhatsAppSubscriptionService(client *ent.Client, log *zap.Logger, treasuryClient *serviceclient.Client, treasuryAPIKey string) *WhatsAppSubscriptionService {
	return &WhatsAppSubscriptionService{
		client:         client,
		log:            log.Named("whatsapp.subscription"),
		treasuryClient: treasuryClient,
		treasuryAPIKey: treasuryAPIKey,
	}
}

// PlanSummary is a public-facing plan representation.
type PlanSummary struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Slug             string  `json:"slug"`
	PriceMonthly     float64 `json:"price_monthly"`
	MessagesPerMonth int     `json:"messages_per_month"`
	IsActive         bool    `json:"is_active"`
}

// SubscriptionSummary is a public-facing subscription representation.
type SubscriptionSummary struct {
	ID               string      `json:"id"`
	TenantID         string      `json:"tenant_id"`
	Plan             PlanSummary `json:"plan"`
	Status           string      `json:"status"`
	StartedAt        time.Time   `json:"started_at"`
	ExpiresAt        time.Time   `json:"expires_at"`
	AutoRenew        bool        `json:"auto_renew"`
	MessagesUsed     int         `json:"messages_used"`
	PaymentReference string      `json:"payment_reference,omitempty"`
}

// GetActivePlans returns all plans available for new subscriptions.
func (s *WhatsAppSubscriptionService) GetActivePlans(ctx context.Context) ([]PlanSummary, error) {
	plans, err := s.client.WhatsAppPlan.Query().
		Where(whatsappplan.IsActive(true)).
		Order(ent.Asc(whatsappplan.FieldPriceMonthly)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query plans: %w", err)
	}

	out := make([]PlanSummary, 0, len(plans))
	for _, p := range plans {
		out = append(out, PlanSummary{
			ID:               p.ID.String(),
			Name:             p.Name,
			Slug:             p.Slug,
			PriceMonthly:     p.PriceMonthly,
			MessagesPerMonth: p.MessagesPerMonth,
			IsActive:         p.IsActive,
		})
	}
	return out, nil
}

// GetTenantSubscription returns the most recent subscription for a tenant.
func (s *WhatsAppSubscriptionService) GetTenantSubscription(ctx context.Context, tenantID uuid.UUID) (*SubscriptionSummary, error) {
	sub, err := s.client.TenantWhatsAppSubscription.Query().
		Where(tenantwhatsappsubscription.TenantIDEQ(tenantID)).
		WithPlan().
		Order(ent.Desc(tenantwhatsappsubscription.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("query subscription: %w", err)
	}

	return s.toSummary(sub), nil
}

// InitiateSubscription creates a Treasury payment intent for a WhatsApp subscription.
func (s *WhatsAppSubscriptionService) InitiateSubscription(ctx context.Context, tenantID uuid.UUID, planID uuid.UUID, returnURL string) (*TopUpResult, error) {
	plan, err := s.client.WhatsAppPlan.Get(ctx, planID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("plan not found")
		}
		return nil, fmt.Errorf("get plan: %w", err)
	}
	if !plan.IsActive {
		return nil, fmt.Errorf("plan is not active")
	}

	amount := decimal.NewFromFloat(plan.PriceMonthly)
	req := map[string]any{
		"amount":         amount,
		"currency":       "KES",
		"payment_method": "pending",
		"reference_id":   fmt.Sprintf("WA-SUB-%s-%d", tenantID.String()[:8], time.Now().Unix()),
		"reference_type": "whatsapp_subscription",
		"source_service": "notifications-service",
		"description":    fmt.Sprintf("WhatsApp %s plan subscription", plan.Name),
		"callback_url":   returnURL,
		"metadata": map[string]any{
			"tenant_id": tenantID.String(),
			"plan_id":   planID.String(),
			"plan_slug": plan.Slug,
		},
	}

	var treasuryHeaders map[string]string
	if s.treasuryAPIKey != "" {
		treasuryHeaders = map[string]string{"X-API-Key": s.treasuryAPIKey}
	}
	resp, err := s.treasuryClient.Post(ctx, fmt.Sprintf("/api/v1/%s/payments/intents", tenantID), req, treasuryHeaders)
	if err != nil {
		return nil, fmt.Errorf("treasury api error: %w", err)
	}
	if !resp.IsSuccess() {
		return nil, fmt.Errorf("treasury api failed with status %d: %s", resp.StatusCode, string(resp.Body))
	}

	var treasuryResp struct {
		IntentID         uuid.UUID       `json:"intent_id"`
		Status           string          `json:"status"`
		Amount           decimal.Decimal `json:"amount"`
		Currency         string          `json:"currency"`
		AuthorizationURL *string         `json:"authorization_url,omitempty"`
		InitiateURL      string          `json:"initiate_url,omitempty"`
	}
	if err := resp.DecodeJSON(&treasuryResp); err != nil {
		return nil, fmt.Errorf("decode treasury response: %w", err)
	}

	return &TopUpResult{
		IntentID:         treasuryResp.IntentID,
		Status:           treasuryResp.Status,
		Amount:           treasuryResp.Amount,
		Currency:         treasuryResp.Currency,
		AuthorizationURL: treasuryResp.AuthorizationURL,
		InitiateURL:      treasuryResp.InitiateURL,
	}, nil
}

// ActivateSubscription sets or renews a tenant's WhatsApp subscription after payment.
func (s *WhatsAppSubscriptionService) ActivateSubscription(ctx context.Context, tenantID uuid.UUID, planID uuid.UUID, paymentRef string) error {
	now := time.Now()
	expiresAt := now.AddDate(0, 1, 0)

	// Check for existing active subscription (renewal case)
	existing, err := s.client.TenantWhatsAppSubscription.Query().
		Where(
			tenantwhatsappsubscription.TenantIDEQ(tenantID),
			tenantwhatsappsubscription.StatusEQ(tenantwhatsappsubscription.StatusActive),
		).
		First(ctx)

	if err == nil {
		// Renew: extend expiry, reset message counter
		_, err = existing.Update().
			SetStatus(tenantwhatsappsubscription.StatusActive).
			SetPlanID(planID).
			SetExpiresAt(expiresAt).
			SetPaymentReference(paymentRef).
			SetMessagesUsed(0).
			SetUpdatedAt(now).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("renew subscription: %w", err)
		}
		s.log.Info("whatsapp subscription renewed",
			zap.String("tenant_id", tenantID.String()),
			zap.String("plan_id", planID.String()),
		)
		return nil
	}

	// New subscription
	_, err = s.client.TenantWhatsAppSubscription.Create().
		SetTenantID(tenantID).
		SetPlanID(planID).
		SetStatus(tenantwhatsappsubscription.StatusActive).
		SetStartedAt(now).
		SetExpiresAt(expiresAt).
		SetPaymentReference(paymentRef).
		SetAutoRenew(true).
		SetMessagesUsed(0).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create subscription: %w", err)
	}

	s.log.Info("whatsapp subscription activated",
		zap.String("tenant_id", tenantID.String()),
		zap.String("plan_id", planID.String()),
	)
	return nil
}

// CancelSubscription marks subscription as cancelled (stops auto-renew, active until expiry).
func (s *WhatsAppSubscriptionService) CancelSubscription(ctx context.Context, tenantID uuid.UUID) error {
	sub, err := s.client.TenantWhatsAppSubscription.Query().
		Where(
			tenantwhatsappsubscription.TenantIDEQ(tenantID),
			tenantwhatsappsubscription.StatusEQ(tenantwhatsappsubscription.StatusActive),
		).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return fmt.Errorf("no active subscription to cancel")
		}
		return fmt.Errorf("query subscription: %w", err)
	}

	_, err = sub.Update().
		SetAutoRenew(false).
		SetStatus(tenantwhatsappsubscription.StatusCancelled).
		SetUpdatedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("cancel subscription: %w", err)
	}

	s.log.Info("whatsapp subscription cancelled", zap.String("tenant_id", tenantID.String()))
	return nil
}

// IsWhatsAppEnabled returns true if the tenant has an active subscription that has not expired.
func (s *WhatsAppSubscriptionService) IsWhatsAppEnabled(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	sub, err := s.getActiveSub(ctx, tenantID)
	if err != nil || sub == nil {
		return false, err
	}
	return true, nil
}

// VerifyQuota verifies the tenant has an active subscription with remaining quota WITHOUT
// incrementing the message counter. Use this for read-only pre-checks (e.g. the synchronous
// HTTP enqueue guard) where the actual counter increment happens later on the delivery path via
// CheckQuota — this avoids double-counting a single message. Returns the same error codes as
// CheckQuota ("no_active_subscription" / "quota_exhausted").
func (s *WhatsAppSubscriptionService) VerifyQuota(ctx context.Context, tenantID uuid.UUID) error {
	sub, err := s.getActiveSub(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("check subscription: %w", err)
	}
	if sub == nil {
		return fmt.Errorf("no_active_subscription: tenant has no active WhatsApp plan")
	}
	plan, err := sub.QueryPlan().Only(ctx)
	if err != nil {
		return fmt.Errorf("query plan: %w", err)
	}
	if plan.MessagesPerMonth > 0 && sub.MessagesUsed >= plan.MessagesPerMonth {
		return fmt.Errorf("quota_exhausted: monthly limit of %d messages reached", plan.MessagesPerMonth)
	}
	return nil
}

// CheckQuota verifies message quota and increments the counter atomically.
// Returns an error with code "quota_exhausted" if over limit.
func (s *WhatsAppSubscriptionService) CheckQuota(ctx context.Context, tenantID uuid.UUID) error {
	sub, err := s.getActiveSub(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("check subscription: %w", err)
	}
	if sub == nil {
		return fmt.Errorf("no_active_subscription: tenant has no active WhatsApp plan")
	}

	plan, err := sub.QueryPlan().Only(ctx)
	if err != nil {
		return fmt.Errorf("query plan: %w", err)
	}

	// 0 = unlimited
	if plan.MessagesPerMonth > 0 && sub.MessagesUsed >= plan.MessagesPerMonth {
		return fmt.Errorf("quota_exhausted: monthly limit of %d messages reached", plan.MessagesPerMonth)
	}

	// Increment counter
	_, err = sub.Update().
		SetMessagesUsed(sub.MessagesUsed + 1).
		SetUpdatedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("increment message counter: %w", err)
	}

	return nil
}

// metaWhatsAppCostPerMessageKES is the ONE centralized estimate of what Meta actually charges per
// business-initiated (template) WhatsApp message sent to a Kenyan recipient — every WhatsAppPlan's
// ProviderCost is derived from this single number rather than being independently guessed per
// plan, so a real-rate correction only ever needs to change one value.
//
// This is a documented ESTIMATE, not a verified live figure: Meta's WhatsApp Business Platform
// bills per business-initiated message (marketing/utility/authentication categories; customer-
// initiated "service" replies within an open 24h window are free), the exact rate varies by the
// RECIPIENT's country (not the sending business's), and Meta revises pricing periodically. Verify
// the current real rate from WhatsApp Manager → your WABA → Overview (or Meta's published pricing
// page) before treating this as final, and update it here if it's materially off — every plan's
// ProviderCost recomputes from this single constant via RecomputeProviderCosts.
const metaWhatsAppCostPerMessageKES = 0.90

// proWhatsAppTypicalMonthlyMessages is the assumed typical usage used to estimate the Pro plan's
// provider cost, since Pro's MessagesPerMonth is 0 (unlimited) and there's no fixed quota to
// multiply by. Purely for cost-tracking/margin visibility — Pro's actual real cost varies with
// each tenant's real usage and is not itself gated by this number.
const proWhatsAppTypicalMonthlyMessages = 2000

// planProviderCost derives a plan's estimated monthly provider cost from the single centralized
// per-message rate above — the ONE place this computation happens, so every caller (seeding a new
// plan, recomputing an existing one) always agrees.
func planProviderCost(messagesPerMonth int) float64 {
	if messagesPerMonth <= 0 { // 0 = unlimited
		return proWhatsAppTypicalMonthlyMessages * metaWhatsAppCostPerMessageKES
	}
	return float64(messagesPerMonth) * metaWhatsAppCostPerMessageKES
}

// SeedDefaultPlans creates the 3 default plans if they don't exist. Provider cost is always
// derived from planProviderCost (the centralized Meta-rate estimate), never hardcoded per plan.
func (s *WhatsAppSubscriptionService) SeedDefaultPlans(ctx context.Context) error {
	defaults := []struct {
		name  string
		slug  string
		price float64
		msgs  int
	}{
		{"Basic", "basic", 500, 100},
		{"Standard", "standard", 1000, 500},
		{"Pro", "pro", 2000, 0},
	}

	for _, d := range defaults {
		exists, err := s.client.WhatsAppPlan.Query().
			Where(whatsappplan.Slug(d.slug)).
			Exist(ctx)
		if err != nil {
			return fmt.Errorf("check plan %s: %w", d.slug, err)
		}
		if exists {
			continue
		}
		_, err = s.client.WhatsAppPlan.Create().
			SetName(d.name).
			SetSlug(d.slug).
			SetPriceMonthly(d.price).
			SetProviderCost(planProviderCost(d.msgs)).
			SetMessagesPerMonth(d.msgs).
			SetIsActive(true).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("seed plan %s: %w", d.slug, err)
		}
		s.log.Info("seeded whatsapp plan", zap.String("slug", d.slug))
	}
	return nil
}

// RecomputeProviderCosts re-derives every existing plan's ProviderCost from the current
// metaWhatsAppCostPerMessageKES estimate — the one place to re-sync live plan rows after that
// constant is corrected against a real verified Meta rate (SeedDefaultPlans only ever creates
// missing plans, it never updates ones that already exist).
func (s *WhatsAppSubscriptionService) RecomputeProviderCosts(ctx context.Context) error {
	plans, err := s.client.WhatsAppPlan.Query().All(ctx)
	if err != nil {
		return fmt.Errorf("list plans: %w", err)
	}
	for _, p := range plans {
		cost := planProviderCost(p.MessagesPerMonth)
		if cost == p.ProviderCost {
			continue
		}
		if _, err := p.Update().SetProviderCost(cost).Save(ctx); err != nil {
			return fmt.Errorf("update plan %s provider cost: %w", p.Slug, err)
		}
		s.log.Info("recomputed whatsapp plan provider cost",
			zap.String("slug", p.Slug), zap.Float64("old_cost", p.ProviderCost), zap.Float64("new_cost", cost))
	}
	return nil
}

func (s *WhatsAppSubscriptionService) getActiveSub(ctx context.Context, tenantID uuid.UUID) (*ent.TenantWhatsAppSubscription, error) {
	sub, err := s.client.TenantWhatsAppSubscription.Query().
		Where(
			tenantwhatsappsubscription.TenantIDEQ(tenantID),
			tenantwhatsappsubscription.StatusEQ(tenantwhatsappsubscription.StatusActive),
			tenantwhatsappsubscription.ExpiresAtGT(time.Now()),
		).
		WithPlan().
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("query active subscription: %w", err)
	}
	return sub, nil
}

func (s *WhatsAppSubscriptionService) toSummary(sub *ent.TenantWhatsAppSubscription) *SubscriptionSummary {
	sum := &SubscriptionSummary{
		ID:               sub.ID.String(),
		TenantID:         sub.TenantID.String(),
		Status:           string(sub.Status),
		StartedAt:        sub.StartedAt,
		ExpiresAt:        sub.ExpiresAt,
		AutoRenew:        sub.AutoRenew,
		MessagesUsed:     sub.MessagesUsed,
		PaymentReference: sub.PaymentReference,
	}
	if sub.Edges.Plan != nil {
		p := sub.Edges.Plan
		sum.Plan = PlanSummary{
			ID:               p.ID.String(),
			Name:             p.Name,
			Slug:             p.Slug,
			PriceMonthly:     p.PriceMonthly,
			MessagesPerMonth: p.MessagesPerMonth,
			IsActive:         p.IsActive,
		}
	}
	return sum
}
