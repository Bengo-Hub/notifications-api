// Package credits is notifications-api's view of the shared credits engine owned by
// subscriptions-api. SMS and WhatsApp balances, tiers, bundles and overage all live there; this
// package only asks "may I send" and "charge this send" over service-to-service calls.
package credits

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"github.com/google/uuid"
)

// Service codes understood by subscriptions-api.
const (
	ServiceSMS      = "sms"
	ServiceWhatsApp = "whatsapp"
)

// Reason codes returned by subscriptions-api when a send is refused.
const (
	ReasonNoSubscription      = "no_subscription"
	ReasonServiceNotActivated = "service_not_activated"
	ReasonInsufficient        = "insufficient_credits"
	ReasonOverageCap          = "overage_cap"
)

// Decision is subscriptions-api's answer to a check or consume call.
type Decision struct {
	Allowed bool   `json:"allowed"`
	Exempt  bool   `json:"exempt"`
	Reason  string `json:"reason"`

	FromQuota  int64   `json:"from_quota"`
	FromBundle int64   `json:"from_bundle"`
	Overage    int64   `json:"overage_units"`
	OverageKES float64 `json:"overage_kes"`
	Replayed   bool    `json:"replayed"`
}

// Charged reports whether the call actually spent units, so the caller knows a refund is due if
// the provider then fails.
func (d Decision) Charged() bool {
	return d.Allowed && !d.Exempt
}

// Client talks to subscriptions-api.
type Client struct {
	sc     *serviceclient.Client
	apiKey string
}

// New creates a client. apiKey is the platform's internal service key.
func New(sc *serviceclient.Client, apiKey string) *Client {
	return &Client{sc: sc, apiKey: apiKey}
}

func (c *Client) headers() map[string]string {
	return map[string]string{"X-API-Key": c.apiKey}
}

// Consume spends units for one send. key must be stable for the send attempt: a repeat with the
// same key never charges twice. An error means the answer is unknown, and the caller must not
// send, so a failing billing service never causes free sends.
func (c *Client) Consume(ctx context.Context, tenantID uuid.UUID, service string, units int64, key, description string, platformScope bool) (Decision, error) {
	body := map[string]any{
		"tenant_id": tenantID.String(), "units": units, "key": key,
		"description": description, "platform_scope": platformScope,
	}
	resp, err := c.sc.Post(ctx, fmt.Sprintf("/api/v1/s2s/credits/%s/consume", service), body, c.headers())
	if err != nil {
		return Decision{}, fmt.Errorf("credits consume: %w", err)
	}
	if !resp.IsSuccess() {
		return Decision{}, fmt.Errorf("credits consume: status %d", resp.StatusCode)
	}
	var d Decision
	if err := resp.DecodeJSON(&d); err != nil {
		return Decision{}, fmt.Errorf("credits consume decode: %w", err)
	}
	return d, nil
}

// Refund reverses the spend made under consumeKey after the provider rejected the message.
func (c *Client) Refund(ctx context.Context, tenantID uuid.UUID, service, consumeKey string) error {
	body := map[string]any{
		"tenant_id": tenantID.String(), "consume_key": consumeKey, "refund_key": consumeKey + ":refund",
	}
	resp, err := c.sc.Post(ctx, fmt.Sprintf("/api/v1/s2s/credits/%s/refund", service), body, c.headers())
	if err != nil {
		return fmt.Errorf("credits refund: %w", err)
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("credits refund: status %d", resp.StatusCode)
	}
	return nil
}

// Check asks whether a send would be allowed, without spending. Used by the HTTP API to answer
// with a 402 straight away; the worker's Consume remains the real decision.
func (c *Client) Check(ctx context.Context, tenantID uuid.UUID, service string, units int64, platformScope bool) (Decision, error) {
	q := url.Values{}
	q.Set("tenant_id", tenantID.String())
	q.Set("units", strconv.FormatInt(units, 10))
	if platformScope {
		q.Set("platform_scope", "true")
	}
	resp, err := c.sc.Get(ctx, fmt.Sprintf("/api/v1/s2s/credits/%s/check?%s", service, q.Encode()), c.headers())
	if err != nil {
		return Decision{}, fmt.Errorf("credits check: %w", err)
	}
	if !resp.IsSuccess() {
		return Decision{}, fmt.Errorf("credits check: status %d", resp.StatusCode)
	}
	var d Decision
	if err := resp.DecodeJSON(&d); err != nil {
		return Decision{}, fmt.Errorf("credits check decode: %w", err)
	}
	return d, nil
}

// RefusalMessage turns a refusal reason into text a sender can act on.
func RefusalMessage(service, reason string) string {
	label := "SMS"
	if service == ServiceWhatsApp {
		label = "WhatsApp"
	}
	switch reason {
	case ReasonNoSubscription:
		return "the sender has no active subscription"
	case ReasonServiceNotActivated:
		return fmt.Sprintf("%s is not activated: buy a %s bundle or plan first", label, label)
	case ReasonInsufficient:
		return fmt.Sprintf("the sender has no %s units left", label)
	case ReasonOverageCap:
		return fmt.Sprintf("unpaid %s overage reached the limit: pay your invoice or buy a bundle", label)
	}
	return fmt.Sprintf("%s sending is not allowed for this sender", label)
}
