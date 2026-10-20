// Package maildelivery turns the mail server's delivery events into delivery log updates. The worker
// marks an email "sent" as soon as the mail server accepts it, which says nothing about whether the
// recipient's server did. Stalwart posts a signed webhook when a delivery fails for good; this package
// verifies it, marks the matching delivery log row as bounced and stops further sends to an address
// that does not exist.
package maildelivery

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/notifications-api/internal/providers/email"
)

// EventDeliveryFailed is the only event the webhook subscribes to: a message that could not be
// delivered and will not be retried.
const EventDeliveryFailed = "delivery.failed"

// Event is one entry of a Stalwart webhook batch.
type Event struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	CreatedAt time.Time      `json:"createdAt"`
	Data      map[string]any `json:"data"`
}

// ParseEvents reads a webhook body, which is {"events":[...]} (a bare array is accepted too).
func ParseEvents(body []byte) ([]Event, error) {
	var wrapped struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Events != nil {
		return wrapped.Events, nil
	}
	var bare []Event
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("maildelivery: body is not a webhook event batch: %w", err)
	}
	return bare, nil
}

// Bounce is a permanent delivery failure for one recipient.
type Bounce struct {
	Recipient string
	// Token is the request id the worker put in the message's Message-ID, or "" when the message was
	// not sent by the worker (for example a manual test).
	Token string
	// Status is the SMTP code the remote server answered, 0 when it was not reported.
	Status int
	// Enhanced is the enhanced status such as 5.1.1, "" when absent.
	Enhanced string
	Detail   string
	// BadAddress is true when the failure says the mailbox or domain does not exist, which is
	// worth remembering so nothing mails it again.
	BadAddress bool
}

var enhancedCode = regexp.MustCompile(`\b[245]\.\d{1,3}\.\d{1,3}\b`)

// BounceFrom classifies an event. It returns false for anything that is not a permanent failure.
func BounceFrom(e Event) (Bounce, bool) {
	if e.Type != EventDeliveryFailed {
		return Bounce{}, false
	}
	detail := firstText(e.Data, "details", "reason", "response", "error")
	status := intOf(e.Data["code"])
	enhanced := enhancedCode.FindString(detail)
	if enhanced == "" {
		enhanced = enhancedCode.FindString(firstText(e.Data, "code", "enhancedCode"))
	}
	permanent := (status >= 500 && status < 600) || strings.HasPrefix(enhanced, "5.")
	if !permanent {
		return Bounce{}, false
	}
	b := Bounce{
		Recipient: strings.ToLower(strings.TrimSpace(firstText(e.Data, "to", "recipient", "rcpt"))),
		Token:     email.MessageIDToken(firstText(e.Data, "messageId", "message-id", "messageID")),
		Status:    status,
		Enhanced:  enhanced,
		Detail:    truncate(detail, 300),
	}
	if b.Recipient == "" {
		return Bounce{}, false
	}
	b.BadAddress = strings.HasPrefix(enhanced, "5.1.") || strings.Contains(strings.ToLower(detail), "user unknown") ||
		strings.Contains(strings.ToLower(detail), "no such user") || strings.Contains(strings.ToLower(detail), "does not exist")
	return b, true
}

// firstText returns the first present key as text. Stalwart sends some values as lists (the "to" of a
// message with several recipients) or numbers, so each shape is flattened.
func firstText(data map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := data[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.Itoa(int(v))
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
