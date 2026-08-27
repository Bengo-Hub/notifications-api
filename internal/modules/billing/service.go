package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/credittransaction"
	"github.com/bengobox/notifications-api/internal/ent/tenantcredit"

	"entgo.io/ent/dialect/sql"
	serviceclient "github.com/Bengo-Hub/shared-service-client"
)

// Service handles credit-based billing for SMS and WhatsApp.
type Service struct {
	client         *ent.Client
	log            *zap.Logger
	segment        *SegmentService
	treasuryClient *serviceclient.Client
	treasuryAPIKey string
}

// NewService creates a new billing service. treasuryAPIKey is the shared INTERNAL_SERVICE_KEY
// sent as X-API-Key on S2S calls to treasury-api (mirrors NewWhatsAppSubscriptionService's
// identical param — InitiateTopUp calls the exact same treasury payment-intents endpoint and
// needs the same auth, which it was never given: a live top-up attempt 401'd at treasury with
// "missing bearer token or API key" before this was wired in).
// resolveTenantName looks up the paying tenant's display name from the local tenant projection
// (already populated for any tenant actively using the platform — see tenant.Syncer) so the
// treasury payment intent carries a real customer_name instead of showing blank on the
// Transactions page. Platform-owner-initiated purchases (SMS top-up, WhatsApp subscription) are
// billed to whichever tenantID the caller actually resolved — including a tenant a platform admin
// is acting on behalf of via the tenant switcher — so this always reflects the real payer, never
// the acting admin's own identity. Best-effort: an unsynced/unknown tenant just leaves the intent
// without a customer_name, same as before this existed.
func resolveTenantName(ctx context.Context, client *ent.Client, tenantID uuid.UUID) string {
	t, err := client.Tenant.Get(ctx, tenantID)
	if err != nil || t == nil {
		return ""
	}
	return t.Name
}

func NewService(client *ent.Client, log *zap.Logger, treasuryClient *serviceclient.Client, treasuryAPIKey string) *Service {
	return &Service{
		client:         client,
		log:            log.Named("billing.service"),
		segment:        NewSegmentService(),
		treasuryClient: treasuryClient,
		treasuryAPIKey: treasuryAPIKey,
	}
}

// TopUpInput defines the payload for initiating a top-up.
type TopUpInput struct {
	TenantID   uuid.UUID       `json:"tenant_id"`
	CreditType string          `json:"credit_type"` // SMS | WHATSAPP
	Amount     decimal.Decimal `json:"amount"`      // Monetary amount in KES
	ReturnURL  string          `json:"return_url,omitempty"`
}

// TopUpResult contains payment intent info.
type TopUpResult struct {
	IntentID         uuid.UUID       `json:"intent_id"`
	Status           string          `json:"status"`
	Amount           decimal.Decimal `json:"amount"`
	Currency         string          `json:"currency"`
	AuthorizationURL *string         `json:"authorization_url,omitempty"`
	InitiateURL      string          `json:"initiate_url,omitempty"`
}

// InitiateTopUp creates a payment intent in Treasury for buying credits.
func (s *Service) InitiateTopUp(ctx context.Context, in TopUpInput) (*TopUpResult, error) {
	if in.Amount.IsNegative() || in.Amount.IsZero() {
		return nil, fmt.Errorf("invalid amount: %s", in.Amount)
	}

	metadata := map[string]any{
		"tenant_id":   in.TenantID.String(),
		"credit_type": in.CreditType,
	}
	// customer_name is how the Transactions page's list resolves who a payment was for
	// (intentCustomerName probes this exact metadata key) — without it, a platform-scoped
	// purchase like this one shows a blank "—" customer. Resolves whichever tenant is actually
	// paying, including a tenant a platform admin is acting on behalf of via the tenant switcher.
	if name := resolveTenantName(ctx, s.client, in.TenantID); name != "" {
		metadata["customer_name"] = name
	}
	req := map[string]any{
		"amount":         in.Amount,
		"currency":       "KES",
		"payment_method": "pending",
		"reference_id":   fmt.Sprintf("TOP-%s-%d", in.TenantID.String()[:8], time.Now().Unix()),
		"reference_type": "topup",
		"source_service": "notifications-service",
		"description":    fmt.Sprintf("Credit top-up for %s", in.CreditType),
		"callback_url":   in.ReturnURL,
		"metadata":       metadata,
	}

	var treasuryHeaders map[string]string
	if s.treasuryAPIKey != "" {
		treasuryHeaders = map[string]string{"X-API-Key": s.treasuryAPIKey}
	}
	resp, err := s.treasuryClient.Post(ctx, fmt.Sprintf("/api/v1/%s/payments/intents", in.TenantID), req, treasuryHeaders)
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

// rateInfo holds both tenant-facing rate and underlying provider cost.
type rateInfo struct {
	Rate         float64
	ProviderCost float64
}

// getSMSRateInfo resolves the SMS rate + provider cost for a tenant (tenant-specific TenantCredit
// override, else the platform PlatformBilling default, else a hardcoded last-resort fallback).
// Credits are an SMS-only wallet — WhatsApp is billed via subscription plans instead (see
// WhatsAppSubscriptionService / docs/sprints/notifications-billing-sprint.md), never a per-message
// credit deduction, so this is SMS-specific rather than generically parameterized by channel.
func (s *Service) getSMSRateInfo(ctx context.Context, tenantID uuid.UUID) (rateInfo, error) {
	// Try tenant-specific rate first
	tc, err := s.client.TenantCredit.Query().
		Where(
			tenantcredit.TenantIDEQ(tenantID),
			tenantcredit.TypeEQ(tenantcredit.TypeSMS),
		).
		Only(ctx)
	if err == nil && tc.Rate > 0 {
		// Tenant override — provider cost unknown; use platform default
		pb, _ := s.client.PlatformBilling.Query().First(ctx)
		var provCost float64
		if pb != nil {
			provCost = pb.ProviderCostPerSms
		}
		return rateInfo{Rate: tc.Rate, ProviderCost: provCost}, nil
	}

	// Platform default
	pb, err := s.client.PlatformBilling.Query().First(ctx)
	if err != nil {
		return rateInfo{Rate: 1.0, ProviderCost: 0.5}, nil
	}
	return rateInfo{Rate: pb.CostPerSms, ProviderCost: pb.ProviderCostPerSms}, nil
}

// DeductSMSCredits calculates segments and deducts credits for SMS delivery using resolved rates.
func (s *Service) DeductSMSCredits(ctx context.Context, tenantID uuid.UUID, body string, recipientCount int, description string) error {
	segments := s.segment.CountSMSSegments(body)
	ri, err := s.getSMSRateInfo(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("resolve rate: %w", err)
	}

	units := float64(segments * recipientCount)
	totalAmount := ri.Rate * units
	providerCost := ri.ProviderCost * units

	s.log.Debug("deducting sms credits",
		zap.String("tenant_id", tenantID.String()),
		zap.Int("segments", segments),
		zap.Int("recipients", recipientCount),
		zap.Float64("rate", ri.Rate),
		zap.Float64("total_amount", totalAmount),
		zap.Float64("provider_cost", providerCost),
	)

	return s.deductCreditsWithCost(ctx, tenantID, "SMS", totalAmount, providerCost, description)
}

// TotalOutstandingBalance sums every tenant's wallet balance for a credit type (e.g. "SMS") —
// the platform's total outstanding, purchased-but-not-yet-spent obligation. Compared against the
// real provider account balance (converted to a KES-equivalent via getRateInfo's provider cost)
// this tells us whether tenant demand has outgrown what the platform's own real wallet can cover.
func (s *Service) TotalOutstandingBalance(ctx context.Context, creditType string) (float64, error) {
	rows, err := s.client.TenantCredit.Query().
		Where(tenantcredit.TypeEQ(tenantcredit.Type(creditType))).
		All(ctx)
	if err != nil {
		return 0, fmt.Errorf("sum outstanding balances: %w", err)
	}
	var total float64
	for _, r := range rows {
		total += r.Balance
	}
	return total, nil
}

// PlatformCostPerSms returns the current platform-wide tenant-facing SMS rate (KES/SMS), falling
// back to the same hardcoded default getRateInfo uses when no PlatformBilling row exists yet.
func (s *Service) PlatformCostPerSms(ctx context.Context) float64 {
	pb, err := s.client.PlatformBilling.Query().First(ctx)
	if err != nil {
		return 1.0
	}
	return pb.CostPerSms
}

// PlatformProviderCost returns the current platform-wide provider cost per unit for a credit type
// (e.g. the real ~KES 0.8/SMS Africa's Talking charges), falling back to the same hardcoded
// defaults getRateInfo uses when no PlatformBilling row exists yet.
func (s *Service) PlatformProviderCost(ctx context.Context, creditType string) float64 {
	pb, err := s.client.PlatformBilling.Query().First(ctx)
	if err != nil {
		if creditType == "SMS" {
			return 0.5
		}
		return 0.8
	}
	if creditType == "SMS" {
		return pb.ProviderCostPerSms
	}
	return pb.ProviderCostPerWhatsapp
}

// GetBalance retrieves the balance for a tenant and credit type.
func (s *Service) GetBalance(ctx context.Context, tenantID uuid.UUID, creditType string) (float64, error) {
	tc, err := s.client.TenantCredit.Query().
		Where(
			tenantcredit.TenantIDEQ(tenantID),
			tenantcredit.TypeEQ(tenantcredit.Type(creditType)),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("query balance: %w", err)
	}
	return tc.Balance, nil
}

// DeductCredits subtracts a fixed amount from a tenant's balance atomically (no provider cost tracking).
func (s *Service) DeductCredits(ctx context.Context, tenantID uuid.UUID, creditType string, amount float64, description string) error {
	return s.deductCreditsWithCost(ctx, tenantID, creditType, amount, 0, description)
}

// deductCreditsWithCost subtracts credits and logs provider cost + platform fee.
func (s *Service) deductCreditsWithCost(ctx context.Context, tenantID uuid.UUID, creditType string, amount, providerCost float64, description string) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("start transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	tc, err := tx.TenantCredit.Query().
		Where(
			tenantcredit.TenantIDEQ(tenantID),
			tenantcredit.TypeEQ(tenantcredit.Type(creditType)),
		).
		Modify(func(s *sql.Selector) {
			s.ForUpdate()
		}).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return fmt.Errorf("insufficient_credits: no %s credit record for tenant", creditType)
		}
		return fmt.Errorf("query credit: %w", err)
	}

	if tc.Balance < amount {
		err = fmt.Errorf("insufficient_credits: balance %.2f < required %.2f", tc.Balance, amount)
		return err
	}

	newBalance := tc.Balance - amount
	_, err = tx.TenantCredit.UpdateOne(tc).
		SetBalance(newBalance).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("update balance: %w", err)
	}

	platformFee := amount - providerCost
	if platformFee < 0 {
		platformFee = 0
	}

	_, err = tx.CreditTransaction.Create().
		SetTenantID(tenantID).
		SetType(credittransaction.Type(creditType)).
		SetAction(credittransaction.ActionDEDUCTION).
		SetAmount(amount).
		SetNewBalance(newBalance).
		SetProviderCost(providerCost).
		SetPlatformFee(platformFee).
		SetDescription(description).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("log deduction transaction: %w", err)
	}

	return tx.Commit()
}

// SMSMarginSummary summarizes realized SMS margin (what tenants were charged vs. the real
// Africa's Talking provider cost) over a period — the data was always captured per-transaction
// (ProviderCost/PlatformFee, see deductCreditsWithCost) but never surfaced anywhere; this is the
// first reader.
type SMSMarginSummary struct {
	Revenue          float64 `json:"revenue"`       // sum of amount charged to tenants
	ProviderCost     float64 `json:"provider_cost"` // sum of the real Africa's Talking cost
	Margin           float64 `json:"margin"`        // revenue - provider_cost
	TransactionCount int     `json:"transaction_count"`
}

// GetSMSMargin sums realized SMS margin over [from, to) (both optional — nil means unbounded)
// across all tenants, or a single tenant when tenantID is non-nil. Platform-admin only; the
// underlying figures reveal the platform's real cost basis, not something a tenant should see.
func (s *Service) GetSMSMargin(ctx context.Context, tenantID *uuid.UUID, from, to *time.Time) (SMSMarginSummary, error) {
	q := s.client.CreditTransaction.Query().
		Where(
			credittransaction.TypeEQ(credittransaction.TypeSMS),
			credittransaction.ActionEQ(credittransaction.ActionDEDUCTION),
		)
	if tenantID != nil {
		q = q.Where(credittransaction.TenantIDEQ(*tenantID))
	}
	if from != nil {
		q = q.Where(credittransaction.CreatedAtGTE(*from))
	}
	if to != nil {
		q = q.Where(credittransaction.CreatedAtLT(*to))
	}

	txs, err := q.All(ctx)
	if err != nil {
		return SMSMarginSummary{}, fmt.Errorf("query sms deductions: %w", err)
	}

	var summary SMSMarginSummary
	for _, tx := range txs {
		summary.Revenue += tx.Amount
		summary.ProviderCost += tx.ProviderCost
	}
	summary.Margin = summary.Revenue - summary.ProviderCost
	summary.TransactionCount = len(txs)
	return summary, nil
}

// CreditTransactionEntry is a summarized credit transaction for the API.
type CreditTransactionEntry struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Type        string    `json:"type"`
	Action      string    `json:"action"`
	Amount      float64   `json:"amount"`
	NewBalance  float64   `json:"new_balance"`
	ReferenceID string    `json:"reference_id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListTransactions returns paginated credit transactions for a tenant.
func (s *Service) ListTransactions(ctx context.Context, tenantID uuid.UUID, creditType string, limit, offset int) ([]CreditTransactionEntry, int, error) {
	q := s.client.CreditTransaction.Query().
		Where(credittransaction.TenantIDEQ(tenantID)).
		Order(ent.Desc(credittransaction.FieldCreatedAt))

	if creditType != "" {
		q = q.Where(credittransaction.TypeEQ(credittransaction.Type(creditType)))
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count transactions: %w", err)
	}

	rows, err := q.Offset(offset).Limit(limit).All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list transactions: %w", err)
	}

	entries := make([]CreditTransactionEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, CreditTransactionEntry{
			ID:          r.ID.String(),
			TenantID:    r.TenantID.String(),
			Type:        string(r.Type),
			Action:      string(r.Action),
			Amount:      r.Amount,
			NewBalance:  r.NewBalance,
			ReferenceID: r.ReferenceID,
			Description: r.Description,
			CreatedAt:   r.CreatedAt,
		})
	}
	return entries, total, nil
}

// TopUpCredits adds credits to a tenant's balance (triggered by payment).
func (s *Service) TopUpCredits(ctx context.Context, tenantID uuid.UUID, creditType string, amount float64, referenceID string) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("start transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	tc, err := tx.TenantCredit.Query().
		Where(
			tenantcredit.TenantIDEQ(tenantID),
			tenantcredit.TypeEQ(tenantcredit.Type(creditType)),
		).
		Modify(func(s *sql.Selector) {
			s.ForUpdate()
		}).
		Only(ctx)

	var newBalance float64
	if err != nil {
		if ent.IsNotFound(err) {
			// Create new record
			newBalance = amount
			_, err = tx.TenantCredit.Create().
				SetTenantID(tenantID).
				SetType(tenantcredit.Type(creditType)).
				SetBalance(newBalance).
				Save(ctx)
			if err != nil {
				return fmt.Errorf("create credit record: %w", err)
			}
		} else {
			return fmt.Errorf("query credit: %w", err)
		}
	} else {
		newBalance = tc.Balance + amount
		_, err = tx.TenantCredit.UpdateOne(tc).
			SetBalance(newBalance).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("update balance: %w", err)
		}
	}

	// Log transaction
	_, err = tx.CreditTransaction.Create().
		SetTenantID(tenantID).
		SetType(credittransaction.Type(creditType)).
		SetAction(credittransaction.ActionTOPUP).
		SetAmount(amount).
		SetNewBalance(newBalance).
		SetReferenceID(referenceID).
		SetDescription(fmt.Sprintf("Top-up via reference %s", referenceID)).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("log transaction: %w", err)
	}

	return tx.Commit()
}
