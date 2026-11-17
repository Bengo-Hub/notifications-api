package ticketing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := serviceclient.DefaultConfig(srv.URL, "ticketing-api", zap.NewNop())
	cfg.MaxElapsedTime = 300 * time.Millisecond
	return New(serviceclient.New(cfg), "test-key")
}

// The call carries the service key, the tenant and the ingest's own source and ref, and reads
// 201 as a new ticket and 200 as the one the message already had.
func TestCreateFromWhatsAppSendsTheIngestKey(t *testing.T) {
	tenant := uuid.New()
	status := http.StatusCreated
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/internal/tickets" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" || r.Header.Get("X-Tenant-ID") != tenant.String() {
			t.Errorf("headers %v", r.Header)
		}
		if r.Header.Get("Idempotency-Key") != "notifications.whatsapp:wa_message:wamid.1" {
			t.Errorf("idempotency key %q", r.Header.Get("Idempotency-Key"))
		}
		var body struct {
			Source    string            `json:"source"`
			SourceRef map[string]string `json:"source_ref"`
			Channel   string            `json:"channel"`
			Phone     string            `json:"requester_phone"`
			PhoneID   string            `json:"phone_number_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Source != "notifications.whatsapp" || body.SourceRef["type"] != "wa_message" || body.SourceRef["id"] != "wamid.1" ||
			body.Channel != "whatsapp" || body.Phone != "+254712345678" || body.PhoneID != "pn-1" {
			t.Errorf("body %+v", body)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"8d3f0c1e-0000-4000-8000-000000000001","ticket_number":"TKT-000012","status":"new"}`))
	})
	req := CreateRequest{Subject: "Order", Description: "Order late", RequesterPhone: "+254712345678", WAMessageID: "wamid.1", PhoneNumberID: "pn-1"}
	got, err := c.CreateFromWhatsApp(context.Background(), tenant, req)
	if err != nil || !got.Created || got.TicketNumber != "TKT-000012" {
		t.Fatalf("first: %+v %v", got, err)
	}
	status = http.StatusOK
	got, err = c.CreateFromWhatsApp(context.Background(), tenant, req)
	if err != nil || got.Created {
		t.Fatalf("repeat: %+v %v", got, err)
	}
}

func TestCreateFromWhatsAppRefusals(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"subject and description are required"}`))
	})
	if _, err := c.CreateFromWhatsApp(context.Background(), uuid.New(), CreateRequest{WAMessageID: "x"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("400: %v", err)
	}
	if _, err := c.CreateFromWhatsApp(context.Background(), uuid.New(), CreateRequest{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("no message id: %v", err)
	}
	if _, err := New(nil, "k").CreateFromWhatsApp(context.Background(), uuid.New(), CreateRequest{WAMessageID: "x"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("not configured: %v", err)
	}
}

func TestTicketURL(t *testing.T) {
	if got := TicketURL("https://ticketing.codevertexafrica.com/", "acme", "abc"); got != "https://ticketing.codevertexafrica.com/acme/tickets/abc" {
		t.Fatalf("url = %s", got)
	}
	if TicketURL("https://ticketing.codevertexafrica.com", "", "abc") != "" {
		t.Fatal("a url without a tenant slug")
	}
}
