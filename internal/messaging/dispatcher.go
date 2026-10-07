package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/bengobox/notifications-api/internal/config"
)

const defaultSubject = "notifications.events"

// ensureStream creates a JetStream stream if it doesn't already exist.
func ensureStream(js nats.JetStreamContext, streamName, subject string) error {
	if streamName == "" {
		streamName = "notifications"
	}
	if subject == "" {
		subject = defaultSubject
	}

	if _, err := js.StreamInfo(streamName); err == nil {
		return nil
	}
	_, err := js.AddStream(&nats.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Retention: nats.LimitsPolicy,
		Storage:   nats.FileStorage,
		MaxAge:    7 * 24 * time.Hour,
		MaxMsgs:   -1,
	})
	return err
}

// streamReady remembers streams already confirmed on this process, so Publish checks (and
// creates) the stream once instead of a StreamInfo round trip before every message.
var streamReady sync.Map

// msgID is the JetStream dedup id for a message: its idempotency key plus channel and recipients.
// Producers reuse one key across channels and recipients of the same event (for example
// "treasury-<event>-<id>" sent by email and WhatsApp), so the key alone would drop real messages;
// only an identical message to the same recipients on the same channel is a duplicate.
func msgID(msg Message) string {
	if msg.IdempotencyKey == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(msg.IdempotencyKey + "|" + msg.Channel + "|" + msg.TemplateID))
	for _, to := range msg.To {
		h.Write([]byte("|" + to))
	}
	// The data too: some keys are time buckets (an OTP resend inside 5 minutes keeps its key but
	// carries a new code). json.Marshal sorts map keys, so equal data hashes equally.
	if data, err := json.Marshal(msg.Data); err == nil {
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Publish enqueues a message to NATS JetStream for asynchronous processing. A message with an
// IdempotencyKey carries msgID as Nats-Msg-Id, so JetStream drops a duplicate publish of the same
// message inside the stream's duplicate window (a retried request or a re-run batch).
func Publish(ctx context.Context, nc *nats.Conn, cfg config.EventsConfig, msg Message) (string, error) {
	if nc == nil {
		return "", fmt.Errorf("event bus not configured")
	}
	js, err := nc.JetStream()
	if err != nil {
		return "", fmt.Errorf("jetstream: %w", err)
	}
	subject := cfg.Subject
	if subject == "" {
		subject = defaultSubject
	}
	streamKey := cfg.StreamName + "|" + subject
	if _, ok := streamReady.Load(streamKey); !ok {
		if err := ensureStream(js, cfg.StreamName, subject); err != nil {
			return "", fmt.Errorf("ensure stream: %w", err)
		}
		streamReady.Store(streamKey, true)
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal message: %w", err)
	}
	header := nats.Header{"Content-Type": []string{"application/json"}}
	if id := msgID(msg); id != "" {
		header.Set(nats.MsgIdHdr, id)
	}
	ack, err := js.PublishMsg(&nats.Msg{
		Subject: subject,
		Data:    payload,
		Header:  header,
	}, nats.Context(ctx))
	if err != nil {
		return "", fmt.Errorf("publish: %w", err)
	}
	return fmt.Sprintf("%d", ack.Sequence), nil
}
