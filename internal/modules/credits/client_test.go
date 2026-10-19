package credits

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	cfg := serviceclient.DefaultConfig(srv.URL, "subscriptions-api", zap.NewNop())
	cfg.MaxElapsedTime = 300 * time.Millisecond
	return New(serviceclient.New(cfg), "test-key")
}

func TestConsumeSendsKeyAndServiceKeyAndDecodesDecision(t *testing.T) {
	tid := uuid.New()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/s2s/credits/sms/consume" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("service key missing")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["tenant_id"] != tid.String() || body["key"] != "n1:a1" || body["units"].(float64) != 3 {
			t.Errorf("body %+v", body)
		}
		_, _ = w.Write([]byte(`{"allowed":true,"from_bundle":2,"overage_units":1,"overage_kes":1}`))
	})
	d, err := c.Consume(context.Background(), tid, ServiceSMS, 3, "n1:a1", "SMS: test", false)
	if err != nil || !d.Allowed || d.FromBundle != 2 || d.Overage != 1 || !d.Charged() {
		t.Fatalf("decision %+v err %v", d, err)
	}
}

func TestExemptDecisionIsNeverCharged(t *testing.T) {
	d := Decision{Allowed: true, Exempt: true}
	if d.Charged() {
		t.Fatal("an exempt tenant has nothing to refund")
	}
	if (Decision{Allowed: false, Reason: ReasonInsufficient}).Charged() {
		t.Fatal("a refusal charged nothing")
	}
}

func TestServerErrorIsAnErrorNotAnAllow(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	d, err := c.Consume(context.Background(), uuid.New(), ServiceWhatsApp, 1, "k", "d", false)
	if err == nil || d.Allowed {
		t.Fatalf("a billing outage must never read as permission to send: %+v %v", d, err)
	}
}

func TestRefusalIsADecisionWithReason(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"allowed":false,"reason":"overage_cap"}`))
	})
	d, err := c.Consume(context.Background(), uuid.New(), ServiceSMS, 1, "k", "d", false)
	if err != nil || d.Allowed || d.Reason != ReasonOverageCap {
		t.Fatalf("%+v %v", d, err)
	}
	if !strings.Contains(RefusalMessage(ServiceSMS, d.Reason), "overage") {
		t.Fatal("message must explain the overage limit")
	}
}

func TestRefundUsesDerivedKeyAndCheckEncodesQuery(t *testing.T) {
	tid := uuid.New()
	var refundBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/refund"):
			_ = json.NewDecoder(r.Body).Decode(&refundBody)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/check"):
			q := r.URL.Query()
			if q.Get("tenant_id") != tid.String() || q.Get("units") != "2" || q.Get("platform_scope") != "" {
				t.Errorf("query %v", q)
			}
			_, _ = w.Write([]byte(`{"allowed":true}`))
		}
	})
	if err := c.Refund(context.Background(), tid, ServiceWhatsApp, "n9:a1"); err != nil {
		t.Fatal(err)
	}
	if refundBody["consume_key"] != "n9:a1" || refundBody["refund_key"] != "n9:a1:refund" {
		t.Fatalf("refund body %+v", refundBody)
	}
	if d, err := c.Check(context.Background(), tid, ServiceSMS, 2, false); err != nil || !d.Allowed {
		t.Fatalf("check %+v %v", d, err)
	}
}

func TestSMSSegments(t *testing.T) {
	long := strings.Repeat("a", 161)
	cases := []struct {
		body string
		want int
	}{
		{"", 0}, {"hello", 1}, {strings.Repeat("a", 160), 1}, {long, 2},
		{strings.Repeat("a", 306), 2}, {strings.Repeat("a", 307), 3},
		{"habari अ", 1}, {strings.Repeat("अ", 71), 2},
	}
	for _, c := range cases {
		if got := SMSSegments(c.body); got != c.want {
			t.Errorf("%d chars: got %d segments, want %d", len([]rune(c.body)), got, c.want)
		}
	}
}
