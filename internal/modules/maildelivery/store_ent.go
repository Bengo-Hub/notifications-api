package maildelivery

import (
	"context"
	"time"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/deliverylog"
)

// lookback bounds the fallback match: a bounce without a usable Message-ID is attributed to the
// newest sent email to that address within the queue's retry life, never to older mail.
const lookback = 7 * 24 * time.Hour

// EntStore is the delivery log backed Store.
type EntStore struct{ client *ent.Client }

// NewEntStore wraps the ent client.
func NewEntStore(client *ent.Client) *EntStore { return &EntStore{client: client} }

// MarkBounced implements Store.
func (s *EntStore) MarkBounced(ctx context.Context, token, recipient string) (int, error) {
	if token != "" {
		n, err := s.client.DeliveryLog.Update().
			Where(
				deliverylog.Channel("email"),
				deliverylog.MessageID(token),
				deliverylog.RecipientEqualFold(recipient),
				deliverylog.StatusNEQ(StatusBounced),
			).
			SetStatus(StatusBounced).
			Save(ctx)
		if err != nil || n > 0 {
			return n, err
		}
	}
	row, err := s.client.DeliveryLog.Query().
		Where(
			deliverylog.Channel("email"),
			deliverylog.Status("sent"),
			deliverylog.RecipientEqualFold(recipient),
			deliverylog.CreatedAtGTE(time.Now().Add(-lookback)),
		).
		Order(ent.Desc(deliverylog.FieldCreatedAt)).
		First(ctx)
	if ent.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return 1, s.client.DeliveryLog.UpdateOneID(row.ID).SetStatus(StatusBounced).Exec(ctx)
}
