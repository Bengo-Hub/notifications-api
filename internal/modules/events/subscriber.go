package events

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

const (
	ackWait    = 30 * time.Second
	maxDeliver = 5
)

// Subscriber listens to cross-service NATS events and dispatches notifications.
type Subscriber struct {
	nc  *nats.Conn
	cfg config.EventsConfig
	log *zap.Logger
}

// New creates a new cross-service event subscriber.
func New(nc *nats.Conn, cfg config.EventsConfig, log *zap.Logger) *Subscriber {
	return &Subscriber{nc: nc, cfg: cfg, log: log.Named("events.subscriber")}
}

// Start subscribes to inventory, KDS, and payroll events.
func (s *Subscriber) Start(ctx context.Context) error {
	js, err := s.nc.JetStream()
	if err != nil {
		return fmt.Errorf("events subscriber: jetstream: %w", err)
	}

	type sub struct {
		subject string
		durable string
		handler nats.MsgHandler
	}

	streams := map[string][]string{
		"inventory": {"inventory.>"},
		"pos":       {"pos.>"},
		"treasury":  {"treasury.>"},
		"erp":       {"erp.>"},
		"logistics": {"logistics.>"},
	}
	for stream, subjects := range streams {
		if _, err := js.StreamInfo(stream); err != nil {
			_, addErr := js.AddStream(&nats.StreamConfig{
				Name:      stream,
				Subjects:  subjects,
				Retention: nats.LimitsPolicy,
				MaxAge:    72 * time.Hour,
				Storage:   nats.FileStorage,
			})
			if addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
				s.log.Warn("events subscriber: ensure stream failed", zap.String("stream", stream), zap.Error(addErr))
			}
		}
	}

	subs := []sub{
		{"inventory.stock.low", "notif-inventory-low-stock", s.handleLowStock},
		{"inventory.purchase_order.received", "notif-po-received", s.handlePOReceived},
		{"inventory.ticket.issued", "notif-ticket-issued", s.handleTicketIssued},
		{"logistics.task.assigned", "notif-rider-task-assigned", s.handleRiderTaskAssigned},
		{"pos.kds.waiter.called", "notif-kds-waiter-called", s.handleKDSWaiterCalled},
		{"pos.loyalty.points.earned", "notif-loyalty-points-earned", s.handleLoyaltyPointsEarned},
		{"pos.loyalty.tier_upgraded", "notif-loyalty-tier-upgraded", s.handleLoyaltyTierUpgraded},
		{"pos.loyalty.referral_earned", "notif-loyalty-referral-earned", s.handleLoyaltyReferralEarned},
		{"treasury.payroll.disbursed", "notif-payroll-disbursed", s.handlePayrollDisbursed},
		{"erp.email.requested", "notif-erp-email-req", s.handleERPEmailRequested},
		{"erp.notification.requested", "notif-erp-notif-req", s.handleERPNotificationRequested},
	}

	for _, s2 := range subs {
		eventslib.SubscribeWithRebind(
			s.log,
			js,
			s2.subject,
			s2.handler,
			nats.Durable(s2.durable),
			nats.AckExplicit(),
			nats.AckWait(ackWait),
			nats.MaxDeliver(maxDeliver),
			nats.DeliverAll(),
		)
	}

	s.log.Info("cross-service NATS subscribers started", zap.Int("count", len(subs)))

	<-ctx.Done()
	return nil
}

func (s *Subscriber) publish(tenantID, channel, templateID, target string, to []string, data map[string]any) {
	msg := messaging.Message{
		TenantID:   tenantID,
		Channel:    channel,
		TemplateID: templateID,
		Target:     target,
		To:         to,
		Data:       data,
		QueuedAt:   time.Now(),
		RequestID:  uuid.New().String(),
	}
	if _, err := messaging.Publish(context.Background(), s.nc, s.cfg, msg); err != nil {
		s.log.Warn("events subscriber: publish notification failed",
			zap.String("template", templateID),
			zap.Error(err),
		)
	}
}

func (s *Subscriber) handleLowStock(msg *nats.Msg) {
	var envelope struct {
		Payload struct {
			TenantID  string  `json:"tenant_id"`
			ItemName  string  `json:"item_name"`
			ItemSKU   string  `json:"item_sku"`
			Quantity  float64 `json:"quantity"`
			Threshold float64 `json:"reorder_point"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		s.log.Warn("low_stock: unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	p := envelope.Payload
	if p.TenantID == "" {
		_ = msg.Ack()
		return
	}
	s.publish(p.TenantID, "email", "inventory/low_stock_alert", messaging.TargetTenantAdmin, nil, map[string]any{
		"item_name":     p.ItemName,
		"item_sku":      p.ItemSKU,
		"quantity":      p.Quantity,
		"reorder_point": p.Threshold,
	})
	s.log.Info("low_stock notification dispatched", zap.String("tenant_id", p.TenantID), zap.String("sku", p.ItemSKU))
	_ = msg.Ack()
}

func (s *Subscriber) handlePOReceived(msg *nats.Msg) {
	var envelope struct {
		Payload struct {
			TenantID     string  `json:"tenant_id"`
			PONumber     string  `json:"po_number"`
			SupplierName string  `json:"supplier_name"`
			TotalAmount  float64 `json:"total_amount"`
			Currency     string  `json:"currency"`
			SupplierEmail string `json:"supplier_email"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		s.log.Warn("po_received: unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	p := envelope.Payload
	if p.TenantID == "" {
		_ = msg.Ack()
		return
	}
	// Notify tenant admin
	s.publish(p.TenantID, "email", "inventory/po_received", messaging.TargetTenantAdmin, nil, map[string]any{
		"po_number":     p.PONumber,
		"supplier_name": p.SupplierName,
		"total_amount":  p.TotalAmount,
		"currency":      p.Currency,
	})
	// Notify supplier if email available
	if p.SupplierEmail != "" {
		s.publish(p.TenantID, "email", "inventory/po_vendor_confirmation", messaging.TargetStaff, []string{p.SupplierEmail}, map[string]any{
			"po_number":     p.PONumber,
			"supplier_name": p.SupplierName,
			"total_amount":  p.TotalAmount,
			"currency":      p.Currency,
		})
	}
	s.log.Info("po_received notifications dispatched", zap.String("po_number", p.PONumber))
	_ = msg.Ack()
}

// handleTicketIssued emails the buyer their event ticket (code + branded PDF link with QR) when
// inventory issues a ticket on a paid order. tenant_id is on the envelope; ticket fields are in the
// payload (see inventory tickets.ticketPayload).
func (s *Subscriber) handleTicketIssued(msg *nats.Msg) {
	var envelope struct {
		TenantID string `json:"tenant_id"`
		Payload  struct {
			BuyerEmail string  `json:"buyer_email"`
			BuyerName  string  `json:"buyer_name"`
			Code       string  `json:"code"`
			TierName   string  `json:"tier_name"`
			Quantity   int     `json:"quantity"`
			TotalPrice float64 `json:"total_price"`
			Currency   string  `json:"currency"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		s.log.Warn("ticket_issued: unmarshal failed", zap.Error(err))
		_ = msg.Ack() // malformed — don't redeliver
		return
	}
	p := envelope.Payload
	if envelope.TenantID == "" || p.BuyerEmail == "" || p.Code == "" {
		// No recipient (e.g. issued before the buyer-contact propagation fix) — nothing to send.
		s.log.Info("ticket_issued: skipping (missing tenant/buyer_email/code)", zap.String("code", p.Code))
		_ = msg.Ack()
		return
	}
	invBase := os.Getenv("INVENTORY_API_URL")
	if invBase == "" {
		invBase = "https://inventoryapi.codevertexitsolutions.com"
	}
	ticketLink := fmt.Sprintf("%s/api/v1/%s/inventory/tickets/%s/pdf", strings.TrimRight(invBase, "/"), envelope.TenantID, p.Code)
	buyerName := p.BuyerName
	if buyerName == "" {
		buyerName = "there"
	}
	s.publish(envelope.TenantID, "email", "events/ticket_issued", messaging.TargetCustomer, []string{p.BuyerEmail}, map[string]any{
		"buyer_name":  buyerName,
		"code":        p.Code,
		"tier_name":   p.TierName,
		"quantity":    p.Quantity,
		"total_price": p.TotalPrice,
		"currency":    p.Currency,
		"ticket_link": ticketLink,
	})
	s.log.Info("ticket_issued notification dispatched",
		zap.String("tenant_id", envelope.TenantID), zap.String("code", p.Code), zap.String("to", p.BuyerEmail))
	_ = msg.Ack()
}

// riderAppURL returns the rider-app base URL from env, defaulting to the canonical production URL.
func riderAppURL() string {
	if u := os.Getenv("NOTIFICATIONS_RIDER_APP_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://riderapp.codevertexitsolutions.com"
}

// handleRiderTaskAssigned emails the rider when a delivery task is assigned to them. tenant_id is on
// the envelope; rider_email/rider_name + tracking/order refs are in the payload (enriched by
// logistics from its synced user table).
func (s *Subscriber) handleRiderTaskAssigned(msg *nats.Msg) {
	var envelope struct {
		TenantID string `json:"tenant_id"`
		Payload  struct {
			TaskID            string `json:"task_id"`
			TrackingCode      string `json:"tracking_code"`
			ExternalReference string `json:"external_reference"`
			RiderEmail        string `json:"rider_email"`
			RiderName         string `json:"rider_name"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		s.log.Warn("rider_task_assigned: unmarshal failed", zap.Error(err))
		_ = msg.Ack()
		return
	}
	p := envelope.Payload
	if envelope.TenantID == "" || p.RiderEmail == "" {
		// No rider email to notify (older event / unsynced rider) — nothing to send.
		s.log.Info("rider_task_assigned: skipping (no tenant/rider_email)", zap.String("task_id", p.TaskID))
		_ = msg.Ack()
		return
	}
	riderName := p.RiderName
	if riderName == "" {
		riderName = "there"
	}
	s.publish(envelope.TenantID, "email", "events/rider_task_assigned", messaging.TargetStaff, []string{p.RiderEmail}, map[string]any{
		"rider_name":    riderName,
		"tracking_code": p.TrackingCode,
		"order_ref":     p.ExternalReference,
		"task_id":       p.TaskID,
		"rider_app_url": riderAppURL(),
	})
	s.log.Info("rider_task_assigned notification dispatched",
		zap.String("tenant_id", envelope.TenantID), zap.String("task_id", p.TaskID), zap.String("to", p.RiderEmail))
	_ = msg.Ack()
}

func (s *Subscriber) handleKDSWaiterCalled(msg *nats.Msg) {
	var envelope struct {
		Payload struct {
			TenantID    string `json:"tenant_id"`
			OrderNumber string `json:"order_number"`
			WaiterPhone string `json:"waiter_phone"`
			WaiterEmail string `json:"waiter_email"`
			TicketID    string `json:"ticket_id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		s.log.Warn("kds_waiter_called: unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	p := envelope.Payload
	if p.TenantID == "" {
		_ = msg.Ack()
		return
	}
	var to []string
	if p.WaiterPhone != "" {
		to = append(to, p.WaiterPhone)
	}
	s.publish(p.TenantID, "sms", "kds/waiter_called", messaging.TargetStaff, to, map[string]any{
		"order_number": p.OrderNumber,
		"ticket_id":    p.TicketID,
	})
	s.log.Info("kds_waiter_called notification dispatched", zap.String("order_number", p.OrderNumber))
	_ = msg.Ack()
}

func (s *Subscriber) handlePayrollDisbursed(msg *nats.Msg) {
	var envelope struct {
		Payload struct {
			TenantID   string  `json:"tenant_id"`
			StaffName  string  `json:"staff_name"`
			StaffPhone string  `json:"staff_phone"`
			Amount     float64 `json:"amount"`
			Currency   string  `json:"currency"`
			Period     string  `json:"period"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		s.log.Warn("payroll_disbursed: unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	p := envelope.Payload
	if p.TenantID == "" {
		_ = msg.Ack()
		return
	}
	var to []string
	if p.StaffPhone != "" {
		to = append(to, p.StaffPhone)
	}
	s.publish(p.TenantID, "sms", "payroll/disbursed", messaging.TargetStaff, to, map[string]any{
		"staff_name": p.StaffName,
		"amount":     p.Amount,
		"currency":   p.Currency,
		"period":     p.Period,
	})
	s.log.Info("payroll_disbursed notification dispatched",
		zap.String("tenant_id", p.TenantID),
		zap.String("staff_name", p.StaffName),
	)
	_ = msg.Ack()
}

// handleLoyaltyPointsEarned sends a "you earned X pts" SMS to the customer (pos.loyalty.points.earned).
func (s *Subscriber) handleLoyaltyPointsEarned(msg *nats.Msg) {
	var e struct {
		TenantID string `json:"tenant_id"`
		Payload  struct {
			TenantID      string `json:"tenant_id"`
			CustomerName  string `json:"customer_name"`
			CustomerPhone string `json:"customer_phone"`
			PointsEarned  int    `json:"points_earned"`
			BalanceAfter  int    `json:"balance_after"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn("loyalty_points_earned: unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	tid := e.TenantID
	if tid == "" {
		tid = e.Payload.TenantID
	}
	if tid == "" || e.Payload.CustomerPhone == "" {
		_ = msg.Ack()
		return
	}
	s.publish(tid, "sms", "pos/loyalty_points_earned", messaging.TargetCustomer, []string{e.Payload.CustomerPhone}, map[string]any{
		"customer_name": e.Payload.CustomerName,
		"points_earned": e.Payload.PointsEarned,
		"balance_after": e.Payload.BalanceAfter,
	})
	_ = msg.Ack()
}

// handleLoyaltyTierUpgraded sends a tier-upgrade congratulations SMS (pos.loyalty.tier_upgraded).
func (s *Subscriber) handleLoyaltyTierUpgraded(msg *nats.Msg) {
	var e struct {
		TenantID string `json:"tenant_id"`
		Payload  struct {
			TenantID       string `json:"tenant_id"`
			CustomerName   string `json:"customer_name"`
			CustomerPhone  string `json:"customer_phone"`
			NewTier        string `json:"new_tier"`
			LifetimePoints int    `json:"lifetime_points"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn("loyalty_tier_upgraded: unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	tid := e.TenantID
	if tid == "" {
		tid = e.Payload.TenantID
	}
	if tid == "" || e.Payload.CustomerPhone == "" {
		_ = msg.Ack()
		return
	}
	s.publish(tid, "sms", "pos/loyalty_tier_upgraded", messaging.TargetCustomer, []string{e.Payload.CustomerPhone}, map[string]any{
		"customer_name":   e.Payload.CustomerName,
		"new_tier":        e.Payload.NewTier,
		"lifetime_points": e.Payload.LifetimePoints,
	})
	_ = msg.Ack()
}

// handleLoyaltyReferralEarned sends a "you earned referral bonus" SMS to the referrer (pos.loyalty.referral_earned).
func (s *Subscriber) handleLoyaltyReferralEarned(msg *nats.Msg) {
	var e struct {
		TenantID string `json:"tenant_id"`
		Payload  struct {
			TenantID      string `json:"tenant_id"`
			ReferrerName  string `json:"referrer_name"`
			ReferrerPhone string `json:"referrer_phone"`
			BonusPoints   int    `json:"bonus_points"`
			BalanceAfter  int    `json:"balance_after"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn("loyalty_referral_earned: unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	tid := e.TenantID
	if tid == "" {
		tid = e.Payload.TenantID
	}
	if tid == "" || e.Payload.ReferrerPhone == "" {
		_ = msg.Ack()
		return
	}
	s.publish(tid, "sms", "pos/loyalty_referral_earned", messaging.TargetCustomer, []string{e.Payload.ReferrerPhone}, map[string]any{
		"customer_name": e.Payload.ReferrerName,
		"bonus_points":  e.Payload.BonusPoints,
		"balance_after": e.Payload.BalanceAfter,
	})
	_ = msg.Ack()
}
