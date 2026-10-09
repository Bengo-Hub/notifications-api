package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
)

// payheroWalletShortTemplate is the platform-ops email for treasury.payhero.service_wallet_short.
const payheroWalletShortTemplate = "platform/payhero_service_wallet_short"

// handlePayHeroServiceWalletShort emails the platform alert recipients when PayHero refuses
// payments because an account's service wallet cannot pay the channel fee. treasury-api publishes
// it at most once per PayHero account per window, so every event is worth a message.
func (s *Subscriber) handlePayHeroServiceWalletShort(msg *nats.Msg) {
	to := s.platformAlertRecipients()
	if len(to) == 0 {
		_ = msg.Ack() // alert deliberately disabled
		return
	}
	var e struct {
		TenantID string `json:"tenant_id"`
		Payload  struct {
			TenantID             string `json:"tenant_id"`
			VendorID             int64  `json:"vendor_id"`
			Mode                 string `json:"mode"`
			Amount               string `json:"amount"`
			Currency             string `json:"currency"`
			ChannelFee           string `json:"channel_fee"`
			ServiceWalletBalance string `json:"service_wallet_balance"`
			Personal             bool   `json:"personal"`
			IntentID             string `json:"intent_id"`
			Reference            string `json:"reference"`
			WindowHours          int    `json:"window_h"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn("payhero_service_wallet_short: unmarshal failed", zap.Error(err))
		_ = msg.Ack() // malformed payload will never parse; retrying is pointless
		return
	}
	p := e.Payload
	tid := e.TenantID
	if tid == "" {
		tid = p.TenantID
	}
	data := map[string]any{
		"tenant_id":              tid,
		"vendor_id":              p.VendorID,
		"mode":                   p.Mode,
		"amount":                 p.Amount,
		"currency":               p.Currency,
		"channel_fee":            p.ChannelFee,
		"service_wallet_balance": p.ServiceWalletBalance,
		"personal":               p.Personal,
		"intent_id":              p.IntentID,
		"reference":              p.Reference,
		"window_h":               p.WindowHours,
		"refused_at":             time.Now().UTC().Format("2006-01-02 15:04 MST"),
		"dashboard_link":         "https://app.payhero.africa",
	}
	subject := fmt.Sprintf("PayHero is refusing payments: account %d service wallet needs a top up", p.VendorID)
	// Platform context, like the new-tenant alert: a tenant's preferences can never mute it.
	s.publishWithMetadata(tid, "email", payheroWalletShortTemplate, messaging.TargetPlatformAdmin, to, data, map[string]any{"subject": subject})
	s.log.Info("payhero_service_wallet_short alert dispatched",
		zap.String("tenant_id", tid), zap.Int64("vendor_id", p.VendorID), zap.Int("recipients", len(to)))
	_ = msg.Ack()
}
