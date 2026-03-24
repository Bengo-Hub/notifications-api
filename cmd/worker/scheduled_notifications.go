package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	serviceclient "github.com/Bengo-Hub/shared-service-client"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// ScheduledNotifier periodically checks for expiring subscriptions and
// dispatches plan expiry warning emails to tenant admins.
type ScheduledNotifier struct {
	logger             *zap.Logger
	subscriptionsClient *serviceclient.Client
	nc                 *nats.Conn
	cfg                *config.Config
	tr                 *tenantResolver
	interval           time.Duration
}

// NewScheduledNotifier creates a new scheduled notifier.
func NewScheduledNotifier(
	logger *zap.Logger,
	subscriptionsClient *serviceclient.Client,
	nc *nats.Conn,
	cfg *config.Config,
	tr *tenantResolver,
) *ScheduledNotifier {
	return &ScheduledNotifier{
		logger:             logger.Named("scheduled-notifier"),
		subscriptionsClient: subscriptionsClient,
		nc:                 nc,
		cfg:                cfg,
		tr:                 tr,
		interval:           6 * time.Hour,
	}
}

// Start begins the background polling loop for scheduled notifications.
func (s *ScheduledNotifier) Start(ctx context.Context) {
	go s.run(ctx)
	s.logger.Info("scheduled notifier started", zap.Duration("interval", s.interval))
}

func (s *ScheduledNotifier) run(ctx context.Context) {
	// Run once immediately on startup
	s.checkExpiringSubscriptions(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduled notifier stopped")
			return
		case <-ticker.C:
			s.checkExpiringSubscriptions(ctx)
		}
	}
}

// expiringSubscription matches the subscriptions-api ListExpiring response.
type expiringSubscription struct {
	TenantID         string `json:"tenant_id"`
	PlanCode         string `json:"plan_code"`
	PlanName         string `json:"plan_name"`
	CurrentPeriodEnd string `json:"current_period_end"`
	DaysRemaining    int    `json:"days_remaining"`
}

// checkExpiringSubscriptions calls subscriptions-api for subscriptions expiring
// within 7, 3, and 1 days, then dispatches plan expiry warning notifications.
func (s *ScheduledNotifier) checkExpiringSubscriptions(ctx context.Context) {
	if s.subscriptionsClient == nil {
		s.logger.Debug("subscriptions client not configured, skipping expiry check")
		return
	}

	// Check 7-day, 3-day, and 1-day warnings
	for _, days := range []int{7, 3, 1} {
		s.sendExpiryWarnings(ctx, days)
	}
}

func (s *ScheduledNotifier) sendExpiryWarnings(ctx context.Context, days int) {
	resp, err := s.subscriptionsClient.Get(ctx, fmt.Sprintf("/api/v1/subscriptions/expiring?days=%d", days), nil)
	if err != nil {
		s.logger.Warn("failed to fetch expiring subscriptions",
			zap.Int("days", days),
			zap.Error(err),
		)
		return
	}

	if !resp.IsSuccess() {
		s.logger.Warn("subscriptions-api returned non-success",
			zap.Int("days", days),
			zap.Int("status", resp.StatusCode),
		)
		return
	}

	var subs []expiringSubscription
	if err := json.Unmarshal(resp.Body, &subs); err != nil {
		s.logger.Warn("failed to decode expiring subscriptions", zap.Error(err))
		return
	}

	if len(subs) == 0 {
		s.logger.Debug("no expiring subscriptions found", zap.Int("days", days))
		return
	}

	for _, sub := range subs {
		// Only send for exact day matches to avoid duplicate notifications
		// e.g. 7-day check only sends for subs with exactly 6-7 days remaining
		if !s.shouldNotifyForDays(sub.DaysRemaining, days) {
			continue
		}

		s.dispatchExpiryWarning(ctx, sub)
	}

	s.logger.Info("expiry warning check completed",
		zap.Int("days", days),
		zap.Int("subscriptions_found", len(subs)),
	)
}

// shouldNotifyForDays returns true if the subscription should receive a notification
// for this check interval. Prevents duplicate notifications across intervals.
func (s *ScheduledNotifier) shouldNotifyForDays(daysRemaining, checkDays int) bool {
	switch checkDays {
	case 7:
		return daysRemaining >= 5 && daysRemaining <= 7
	case 3:
		return daysRemaining >= 2 && daysRemaining <= 4
	case 1:
		return daysRemaining <= 1
	default:
		return false
	}
}

func (s *ScheduledNotifier) dispatchExpiryWarning(ctx context.Context, sub expiringSubscription) {
	// Resolve tenant admin email
	ti, err := s.tr.resolve(ctx, sub.TenantID)
	if err != nil {
		s.logger.Warn("failed to resolve tenant for expiry warning",
			zap.String("tenant_id", sub.TenantID),
			zap.Error(err),
		)
		return
	}

	if ti.ContactEmail == "" {
		s.logger.Debug("tenant has no contact email, skipping expiry warning",
			zap.String("tenant_id", sub.TenantID),
		)
		return
	}

	tenantWebsite := ti.Website
	if tenantWebsite == "" {
		tenantWebsite = "https://pricing.codevertexitsolutions.com"
	}

	msg := messaging.Message{
		TenantID:    sub.TenantID,
		Channel:     "email",
		TemplateID:  "platform/plan_expiry_warning",
		SenderScope: messaging.SenderScopePlatform,
		Target:      messaging.TargetTenantAdmin,
		To:          []string{ti.ContactEmail},
		Data: map[string]any{
			"name":           ti.Name,
			"plan_name":      sub.PlanName,
			"days_remaining": sub.DaysRemaining,
			"expiry_date":    sub.CurrentPeriodEnd,
			"action_link":    fmt.Sprintf("%s/settings/subscription", tenantWebsite),
		},
		Metadata: map[string]any{
			"subject": fmt.Sprintf("Your %s subscription expires in %d days", sub.PlanName, sub.DaysRemaining),
		},
		RequestID:      uuid.New().String(),
		IdempotencyKey: fmt.Sprintf("plan-expiry-%s-%d-%s", sub.TenantID, sub.DaysRemaining, time.Now().Format("2006-01-02")),
		QueuedAt:       time.Now(),
	}

	if _, err := messaging.Publish(ctx, s.nc, s.cfg.Events, msg); err != nil {
		s.logger.Error("failed to dispatch expiry warning notification",
			zap.String("tenant_id", sub.TenantID),
			zap.String("plan", sub.PlanName),
			zap.Error(err),
		)
		return
	}

	s.logger.Info("expiry warning notification dispatched",
		zap.String("tenant_id", sub.TenantID),
		zap.String("plan", sub.PlanName),
		zap.Int("days_remaining", sub.DaysRemaining),
		zap.String("to", ti.ContactEmail),
	)
}
