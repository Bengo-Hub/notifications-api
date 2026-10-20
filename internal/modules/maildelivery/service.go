package maildelivery

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// StatusBounced is the delivery log status of an email the recipient's server refused for good.
const StatusBounced = "bounced"

// SuppressionTTL is how long an address that does not exist is left alone.
const SuppressionTTL = 30 * 24 * time.Hour

// Store records the outcome on the delivery log.
type Store interface {
	// MarkBounced flags the email delivery log rows of one recipient as bounced. token is the request
	// id carried in the Message-ID; when it is empty or matches nothing, the most recent sent email to
	// that recipient inside the lookback window is flagged instead. It returns how many rows changed.
	MarkBounced(ctx context.Context, token, recipient string) (int, error)
}

// Suppressor stops further sends to an address.
type Suppressor func(ctx context.Context, addr string, ttl time.Duration)

// Result counts what one webhook batch did.
type Result struct {
	Events     int
	Bounced    int
	Suppressed int
}

// Service applies delivery events.
type Service struct {
	store    Store
	suppress Suppressor
	log      *zap.Logger
}

// NewService builds the service. suppress may be nil.
func NewService(store Store, suppress Suppressor, log *zap.Logger) *Service {
	return &Service{store: store, suppress: suppress, log: log}
}

// Handle applies a batch. A store error on one event is logged and the rest still run, because the
// mail server resends the whole batch on any non-2xx answer and the work is idempotent.
func (s *Service) Handle(ctx context.Context, events []Event) Result {
	res := Result{Events: len(events)}
	for _, e := range events {
		b, ok := BounceFrom(e)
		if !ok {
			continue
		}
		n, err := s.store.MarkBounced(ctx, b.Token, b.Recipient)
		if err != nil {
			s.log.Warn("mail delivery: could not mark bounce", zap.String("event", e.ID), zap.Error(err))
			continue
		}
		res.Bounced += n
		if b.BadAddress && s.suppress != nil {
			s.suppress(ctx, b.Recipient, SuppressionTTL)
			res.Suppressed++
		}
		s.log.Info("mail delivery bounce",
			zap.Int("rows", n), zap.Int("smtp_status", b.Status), zap.String("enhanced", b.Enhanced),
			zap.Bool("bad_address", b.BadAddress), zap.String("detail", b.Detail))
	}
	return res
}
