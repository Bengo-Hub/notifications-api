package messaging

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// WhatsApp sends to one recipient with backups (metadata fallback_to): Meta accepts a send and
// reports later, by webhook, that it could not deliver it (no WhatsApp on the number, a bad
// number). The worker parks the next attempt under Meta's message id; the webhook takes it when
// that message fails and queues it, so the backup number gets the message only then.

const (
	// MetaFallbackTo lists the numbers to try, in order, when the send to the recipient fails.
	MetaFallbackTo = "fallback_to"
	// MetaSentMessageID is set by the WhatsApp provider to Meta's message id after a send.
	MetaSentMessageID = "sent_message_id"

	whatsAppFallbackPrefix = "notif:wa-fallback:"
	whatsAppFallbackTTL    = 24 * time.Hour
)

// FallbackNumbers reads metadata fallback_to ([]string in process, []any after JSON).
func FallbackNumbers(metadata map[string]any) []string {
	var out []string
	switch v := metadata[MetaFallbackTo].(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	clean := make([]string, 0, len(out))
	for _, s := range out {
		if s = strings.TrimSpace(s); s != "" {
			clean = append(clean, s)
		}
	}
	return clean
}

// NextFallback is msg sent to the first backup number, carrying the rest as its own backups.
// ok is false when there is no backup left.
func NextFallback(msg Message) (Message, bool) {
	nums := FallbackNumbers(msg.Metadata)
	if len(nums) == 0 {
		return Message{}, false
	}
	next := msg
	next.To = []string{nums[0]}
	next.Metadata = make(map[string]any, len(msg.Metadata))
	for k, v := range msg.Metadata {
		next.Metadata[k] = v
	}
	delete(next.Metadata, MetaSentMessageID)
	if len(nums) > 1 {
		next.Metadata[MetaFallbackTo] = nums[1:]
	} else {
		delete(next.Metadata, MetaFallbackTo)
	}
	next.IdempotencyKey = msg.IdempotencyKey + "-fb-" + nums[0]
	return next, true
}

// ParkWhatsAppFallback keeps the next attempt of a sent message until Meta reports the message
// (messageID) failed, for a day at most.
func ParkWhatsAppFallback(ctx context.Context, rdb redis.UniversalClient, messageID string, next Message) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return rdb.Set(ctx, whatsAppFallbackPrefix+messageID, data, whatsAppFallbackTTL).Err()
}

// TakeWhatsAppFallback returns, once, the attempt parked for a failed message.
func TakeWhatsAppFallback(ctx context.Context, rdb redis.UniversalClient, messageID string) (*Message, bool) {
	data, err := rdb.GetDel(ctx, whatsAppFallbackPrefix+messageID).Bytes()
	if err != nil {
		return nil, false
	}
	var m Message
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	return &m, true
}

// DropWhatsAppFallback forgets the parked attempt once the message is delivered.
func DropWhatsAppFallback(ctx context.Context, rdb redis.UniversalClient, messageID string) {
	_ = rdb.Del(ctx, whatsAppFallbackPrefix+messageID).Err()
}
