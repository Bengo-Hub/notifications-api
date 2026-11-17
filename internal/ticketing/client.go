// Package ticketing is notifications-api's view of ticketing-api: the one call the WhatsApp inbox
// makes to turn a customer message into a support ticket. Tickets are owned by ticketing-api;
// nothing about the ticket is stored here.
package ticketing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"github.com/google/uuid"
)

// Source and RefType are the key ticketing-api's WhatsApp ingest gives a ticket opened from a
// message (tickets.MessageKey for the whatsapp channel). Converting a message from the inbox uses
// the same key, so the two paths never open two tickets for one message.
const (
	Source  = "notifications.whatsapp"
	RefType = "wa_message"
)

// ErrDisabled is returned when no ticketing-api URL is configured.
var ErrDisabled = errors.New("ticketing-api is not configured")

// ErrRefused is returned when ticketing-api refuses the request (a 4xx answer).
var ErrRefused = errors.New("ticketing-api refused the ticket")

// CreateRequest is one WhatsApp message to turn into a ticket.
type CreateRequest struct {
	Subject        string
	Description    string
	RequesterName  string
	RequesterPhone string
	// WAMessageID is the message's WhatsApp id, the idempotency key.
	WAMessageID string
	// PhoneNumberID is the tenant number the message came in on; ReceivedAt (RFC 3339) is when.
	PhoneNumberID string
	ReceivedAt    string
}

// Ticket is ticketing-api's answer. Created is false when the message already had a ticket.
type Ticket struct {
	ID           string `json:"id"`
	TicketNumber string `json:"ticket_number"`
	Status       string `json:"status"`
	Created      bool   `json:"-"`
}

// Client calls ticketing-api's internal routes with the platform service key.
type Client struct {
	sc     *serviceclient.Client
	apiKey string
}

// New builds a client. A nil service client gives a client whose calls return ErrDisabled.
func New(sc *serviceclient.Client, apiKey string) *Client {
	return &Client{sc: sc, apiKey: apiKey}
}

// CreateFromWhatsApp opens the ticket for a WhatsApp message, or returns the one it already has
// (opened by the inbox before, or by ticketing-api's own WhatsApp ingest). The call is safe to
// retry: ticketing-api keys it on (source, source_ref), so the Idempotency-Key header lets the
// shared client retry a dropped connection without risk of a second ticket.
func (c *Client) CreateFromWhatsApp(ctx context.Context, tenantID uuid.UUID, in CreateRequest) (*Ticket, error) {
	if c == nil || c.sc == nil {
		return nil, ErrDisabled
	}
	id := strings.TrimSpace(in.WAMessageID)
	if id == "" {
		return nil, fmt.Errorf("%w: the message has no WhatsApp id", ErrRefused)
	}
	body := map[string]any{
		"subject":         in.Subject,
		"description":     in.Description,
		"channel":         "whatsapp",
		"source":          Source,
		"source_ref":      map[string]string{"type": RefType, "id": id},
		"requester_name":  in.RequesterName,
		"requester_phone": in.RequesterPhone,
		"phone_number_id": in.PhoneNumberID,
		"received_at":     in.ReceivedAt,
	}
	headers := map[string]string{
		"X-API-Key":       c.apiKey,
		"X-Tenant-ID":     tenantID.String(),
		"Idempotency-Key": Source + ":" + RefType + ":" + id,
	}
	resp, err := c.sc.Post(ctx, "/api/v1/internal/tickets", body, headers)
	if err != nil {
		return nil, fmt.Errorf("ticketing create: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nil, fmt.Errorf("%w: status %d", ErrRefused, resp.StatusCode)
	default:
		return nil, fmt.Errorf("ticketing create: status %d", resp.StatusCode)
	}
	var t Ticket
	if err := resp.DecodeJSON(&t); err != nil {
		return nil, fmt.Errorf("ticketing create: decode: %w", err)
	}
	if t.ID == "" {
		return nil, errors.New("ticketing create: no ticket id in the answer")
	}
	t.Created = resp.StatusCode == http.StatusCreated
	return &t, nil
}

// TicketURL is the ticket's page in the ticketing console.
func TicketURL(consoleURL, tenantSlug, ticketID string) string {
	base := strings.TrimRight(strings.TrimSpace(consoleURL), "/")
	if base == "" || tenantSlug == "" || ticketID == "" {
		return ""
	}
	return base + "/" + tenantSlug + "/tickets/" + ticketID
}
