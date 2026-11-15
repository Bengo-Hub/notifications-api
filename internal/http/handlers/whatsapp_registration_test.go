package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	httpware "github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
)

// metaSim is a stateful Meta for the register and disconnect workflows. Like the real one, a
// deregistered number keeps platform_type CLOUD_API with status DISCONNECTED, and two-step
// verification keeps the first PIN set, refusing any other with 133005.
type metaSim struct {
	mu         sync.Mutex
	numbers    map[string]*simNumber // phone number ID -> number
	accounts   map[string][]string   // WABA ID -> phone number IDs
	subscribed map[string]bool       // WABA ID -> our webhook subscribed
	calls      []string
}

type simNumber struct {
	status, platformType, pin string
	// needsVerification makes register answer 133006 until a code is verified.
	needsVerification bool
	sentCode          string
	sent              int
	methods           []string
}

func newMetaSim(accounts map[string][]string) *metaSim {
	s := &metaSim{numbers: map[string]*simNumber{}, accounts: accounts, subscribed: map[string]bool{}}
	for _, ids := range accounts {
		for _, id := range ids {
			s.numbers[id] = &simNumber{status: "PENDING", platformType: "NOT_APPLICABLE"}
		}
	}
	return s
}

func (s *metaSim) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")[1:] // drop the version
		b, _ := io.ReadAll(r.Body)
		switch {
		case len(parts) == 2 && parts[0] == "oauth":
			_, _ = w.Write([]byte(`{"access_token":"business-token"}`))
		case len(parts) == 1 && r.Method == http.MethodGet:
			if n, ok := s.numbers[parts[0]]; ok {
				_ = json.NewEncoder(w).Encode(map[string]string{"platform_type": n.platformType, "status": n.status, "display_phone_number": "+254", "verified_name": "Biz", "code_verification_status": codeStatus(n)})
				return
			}
			_, _ = w.Write([]byte(`{"primary_funding_id":"card-1"}`))
		case len(parts) == 2 && parts[1] == "phone_numbers":
			var data []map[string]string
			for _, id := range s.accounts[parts[0]] {
				data = append(data, map[string]string{"id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case len(parts) == 2 && parts[1] == "subscribed_apps":
			s.subscribed[parts[0]] = r.Method == http.MethodPost
			_, _ = w.Write([]byte(`{"success":true}`))
		case len(parts) == 2 && parts[1] == "register":
			var body map[string]string
			_ = json.Unmarshal(b, &body)
			n := s.numbers[parts[0]]
			if n.needsVerification {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"(#133006) Phone number re-verification needed","code":133006}}`))
				return
			}
			if n.pin != "" && body["pin"] != n.pin {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"(#133005) Two step verification PIN Mismatch","code":133005}}`))
				return
			}
			n.pin, n.status, n.platformType = body["pin"], "CONNECTED", "CLOUD_API"
			_, _ = w.Write([]byte(`{"success":true}`))
		case len(parts) == 2 && parts[1] == "request_code":
			var body map[string]string
			_ = json.Unmarshal(b, &body)
			n := s.numbers[parts[0]]
			n.sent++
			n.sentCode = []string{"481516", "234200"}[(n.sent-1)%2]
			n.methods = append(n.methods, body["code_method"])
			_, _ = w.Write([]byte(`{"success":true}`))
		case len(parts) == 2 && parts[1] == "verify_code":
			var body map[string]string
			_ = json.Unmarshal(b, &body)
			n := s.numbers[parts[0]]
			if n.sentCode == "" || body["code"] != n.sentCode {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"(#136025) Verify OTP code failed","code":136025,"error_user_msg":"The code you entered is incorrect"}}`))
				return
			}
			n.needsVerification, n.sentCode = false, ""
			_, _ = w.Write([]byte(`{"success":true}`))
		case len(parts) == 2 && parts[1] == "deregister":
			s.numbers[parts[0]].status = "DISCONNECTED" // platform_type stays CLOUD_API
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			_, _ = w.Write([]byte(`{"success":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *metaSim) number(id string) simNumber {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.numbers[id]
}

const (
	portfolioWABA = "2633992870377940"
	platformPhone = "1216738621531789"
	urbanPhone    = "1342267082299342"
)

// simStore is the provider_settings rows of each tenant's whatsapp/meta_cloud connection.
type simStore struct {
	mu   sync.Mutex
	rows map[string]map[string]string
	pins int
}

// newSimHandler wires the handler to metaSim and simStore the way NewWhatsAppEmbeddedSignupHandler
// wires it to Meta and the database.
func newSimHandler(t *testing.T, base string) (*WhatsAppEmbeddedSignupHandler, *simStore) {
	t.Helper()
	st := &simStore{rows: map[string]map[string]string{}}
	creds := map[string]string{"access_token": "platform-token", "api_version": "v26.0", "app_id": "app-1", "app_secret": "shh",
		"phone_number_id": platformPhone, "waba_id": portfolioWABA}
	h := &WhatsAppEmbeddedSignupHandler{
		log:        zap.NewNop(),
		http:       http.DefaultClient,
		graphBase:  base,
		creds:      func(context.Context) (map[string]string, error) { return creds, nil },
		encrypt:    func(v string) (string, error) { return v, nil },
		platformID: "platform-tenant",
	}
	h.newPIN = func() (string, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.pins++
		return []string{"111111", "222222", "333333", "444444", "555555"}[st.pins%5], nil
	}
	h.save = func(_ context.Context, tenantID string, rows []signupSetting) error {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.rows[tenantID] == nil {
			st.rows[tenantID] = map[string]string{}
		}
		for _, r := range rows {
			st.rows[tenantID][r.key] = r.value
		}
		return nil
	}
	h.removeKeys = func(_ context.Context, tenantID string, keys ...string) error {
		st.mu.Lock()
		defer st.mu.Unlock()
		for _, k := range keys {
			delete(st.rows[tenantID], k)
		}
		return nil
	}
	h.remove = func(ctx context.Context, tenantID string) error {
		return h.removeKeys(ctx, tenantID, connectionKeys...)
	}
	h.conn = func(_ context.Context, tenantID string) (string, string, string, string) {
		st.mu.Lock()
		defer st.mu.Unlock()
		row := st.rows[tenantID]
		token := row["access_token"]
		if row["token_source"] == "platform" {
			token = creds["access_token"]
		}
		return row["phone_number_id"], row["waba_id"], token, "v26.0"
	}
	h.storedPIN = func(_ context.Context, tenantID string) string {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.rows[tenantID]["registration_pin"]
	}
	h.linkedElsewhere = func(context.Context, string, string, string) (bool, error) { return false, nil }
	h.sharedUse = func(_ context.Context, tenantID, phoneID, wabaID string) (bool, bool, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		phone, waba := false, false
		for tenant, row := range st.rows {
			if tenant == tenantID {
				continue
			}
			phone = phone || (phoneID != "" && row["phone_number_id"] == phoneID)
			waba = waba || (wabaID != "" && row["waba_id"] == wabaID)
		}
		return phone, waba, nil
	}
	return h, st
}

// ownerRequest is a platform owner's request acting for target ("" for All Tenants).
func ownerRequest(method, path, target, body string) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if target != "" {
		r.Header.Set("X-Tenant-ID", target)
	}
	ctx := context.WithValue(httpware.WithTenantID(r.Context(), "platform-tenant"), httpware.IsPlatformOwnerKey, true)
	return r.WithContext(ctx)
}

func simLink(h *WhatsAppEmbeddedSignupHandler, target, phone, waba string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ManagedLink(w, ownerRequest(http.MethodPost, "/api/v1/billing/whatsapp/managed-link", target, `{"phone_number_id":"`+phone+`","waba_id":"`+waba+`"}`))
	return w
}

func simDisconnect(h *WhatsAppEmbeddedSignupHandler, target, query string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Disconnect(w, ownerRequest(http.MethodDelete, "/api/v1/billing/whatsapp/connection"+query, target, ""))
	return w
}

// sendReady is what a send needs: the tenant's number, account and a token, with the number
// connected on Cloud API and our webhook subscribed to its account.
func sendReady(t *testing.T, h *WhatsAppEmbeddedSignupHandler, sim *metaSim, tenantID string) {
	t.Helper()
	phone, waba, token, _ := h.conn(context.Background(), tenantID)
	if phone == "" || waba == "" || token == "" {
		t.Fatalf("%s: not linked (phone %q, waba %q, token set %v)", tenantID, phone, waba, token != "")
	}
	if n := sim.number(phone); n.status != "CONNECTED" || n.platformType != "CLOUD_API" {
		t.Fatalf("%s: number %s is %+v on Meta, not connected", tenantID, phone, n)
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	if !sim.subscribed[waba] {
		t.Fatalf("%s: webhook not subscribed to %s", tenantID, waba)
	}
}

func mustOK(t *testing.T, step string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", step, w.Code, w.Body.String())
	}
}

// (a) A portfolio tenant can be disconnected and linked again any number of times, with or
// without releasing the number, while the platform keeps its own number in the same account.
func TestCyclePortfolioTenant(t *testing.T) {
	sim := newMetaSim(map[string][]string{portfolioWABA: {platformPhone, urbanPhone}})
	srv := sim.server(t)
	h, _ := newSimHandler(t, srv.URL)

	mustOK(t, "platform link", simLink(h, "", platformPhone, portfolioWABA))
	for i, release := range []string{"", "?release=false", "?release=true", "?release=true"} {
		mustOK(t, "link", simLink(h, "urbanloft", urbanPhone, portfolioWABA))
		sendReady(t, h, sim, "urbanloft")
		sendReady(t, h, sim, "platform-tenant")
		mustOK(t, "disconnect", simDisconnect(h, "urbanloft", release))
		if p, _, _, _ := h.conn(context.Background(), "urbanloft"); p != "" {
			t.Fatalf("round %d: still linked after Disconnect", i)
		}
		// The shared account keeps our webhook, and the platform keeps sending.
		sendReady(t, h, sim, "platform-tenant")
		wantStatus := "CONNECTED"
		if release == "?release=true" {
			wantStatus = "DISCONNECTED"
		}
		if n := sim.number(urbanPhone); n.status != wantStatus {
			t.Fatalf("round %d (%q): number is %s, want %s", i, release, n.status, wantStatus)
		}
	}
	mustOK(t, "final link", simLink(h, "urbanloft", urbanPhone, portfolioWABA))
	sendReady(t, h, sim, "urbanloft")
}

// (b) The platform tenant: Disconnect removes only its link (Meta untouched), Link with All
// Tenants selected brings it back, and Re-register fixes a number deregistered elsewhere.
func TestCyclePlatformTenant(t *testing.T) {
	sim := newMetaSim(map[string][]string{portfolioWABA: {platformPhone, urbanPhone}})
	srv := sim.server(t)
	h, _ := newSimHandler(t, srv.URL)

	for i := 0; i < 3; i++ {
		mustOK(t, "link", simLink(h, "", platformPhone, portfolioWABA))
		sendReady(t, h, sim, "platform-tenant")
		mustOK(t, "disconnect", simDisconnect(h, "", "?release=true"))
		if p, _, _, _ := h.conn(context.Background(), "platform-tenant"); p != "" {
			t.Fatalf("round %d: still linked", i)
		}
		if n := sim.number(platformPhone); n.status != "CONNECTED" {
			t.Fatalf("round %d: the platform number was released on Meta: %+v", i, n)
		}
		sim.mu.Lock()
		sub := sim.subscribed[portfolioWABA]
		sim.mu.Unlock()
		if !sub {
			t.Fatalf("round %d: the platform account was unsubscribed", i)
		}
	}

	// Deregistered outside this flow (the 15 November state): Re-register fixes it in place.
	mustOK(t, "relink", simLink(h, "", platformPhone, portfolioWABA))
	sim.mu.Lock()
	sim.numbers[platformPhone].status = "DISCONNECTED"
	sim.mu.Unlock()
	w := httptest.NewRecorder()
	h.Reregister(w, ownerRequest(http.MethodPost, "/api/v1/billing/whatsapp/reregister", "", ""))
	mustOK(t, "reregister", w)
	sendReady(t, h, sim, "platform-tenant")
}

// (c) An Embedded Signup tenant: Disconnect releases its own number and account, and connecting
// again registers the number with the PIN set the first time.
func TestCycleEmbeddedSignupTenant(t *testing.T) {
	sim := newMetaSim(map[string][]string{"W9": {"P9"}})
	srv := sim.server(t)
	h, _ := newSimHandler(t, srv.URL)
	complete := func() {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/billing/whatsapp/embedded-signup/complete",
			bytes.NewBufferString(`{"code":"c","waba_id":"W9","phone_number_id":"P9"}`))
		w := httptest.NewRecorder()
		h.Complete(w, r.WithContext(httpware.WithTenantID(r.Context(), "shop")))
		mustOK(t, "complete", w)
	}
	for i := 0; i < 3; i++ {
		complete()
		sendReady(t, h, sim, "shop")
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, "/api/v1/billing/whatsapp/connection", nil)
		h.Disconnect(w, r.WithContext(httpware.WithTenantID(r.Context(), "shop")))
		mustOK(t, "disconnect", w)
		if n := sim.number("P9"); n.status != "DISCONNECTED" {
			t.Fatalf("round %d: own number not released: %+v", i, n)
		}
	}
	complete()
	sendReady(t, h, sim, "shop")
}

// A PIN refused by two-step verification says how to fix it.
func TestRegisterPINMismatchSaysWhatToDo(t *testing.T) {
	sim := newMetaSim(map[string][]string{portfolioWABA: {urbanPhone}})
	sim.numbers[urbanPhone] = &simNumber{status: "DISCONNECTED", platformType: "CLOUD_API", pin: "999999"}
	srv := sim.server(t)
	h, _ := newSimHandler(t, srv.URL)
	w := simLink(h, "urbanloft", urbanPhone, portfolioWABA)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "reset two-step verification") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

// Meta's own title and message reach the client, with the hint for an unverified number, and the
// token never does.
func TestManagedLinkShowsMetaRefusal(t *testing.T) {
	g := numericLinkGraph()
	g.answers["POST /v26.0/109/register"] = graphAnswer{status: http.StatusBadRequest,
		body: `{"error":{"message":"(#100) Invalid parameter platform-token","code":100,"error_subcode":2593005,"error_user_title":"Phone number not verified","error_user_msg":"Phone number is not verified through SMS or voice"}}`}
	srv := g.server(t)
	h, rec := newLinkHandler(t, srv.URL)
	w := postLink(h, true, linkBody)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["meta_title"] != "Phone number not verified" || body["meta_message"] != "Phone number is not verified through SMS or voice" {
		t.Errorf("body = %v", body)
	}
	if !strings.Contains(body["error"].(string), "verify the number with a code sent by SMS or voice") || body["meta_subcode"] != float64(2593005) || body["needs_verification"] != true {
		t.Errorf("body = %v", body)
	}
	if strings.Contains(w.Body.String(), "platform-token") {
		t.Error("the token was echoed")
	}
	if rec.saved.tenant != "" {
		t.Fatal("saved after Meta refused")
	}
}

func TestReregisterRules(t *testing.T) {
	sim := newMetaSim(map[string][]string{portfolioWABA: {urbanPhone}})
	srv := sim.server(t)
	h, st := newSimHandler(t, srv.URL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/billing/whatsapp/reregister", nil)
	h.Reregister(w, r.WithContext(httpware.WithTenantID(r.Context(), "urbanloft")))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non owner: status %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.Reregister(w, ownerRequest(http.MethodPost, "/x", "urbanloft", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("nothing linked: status %d", w.Code)
	}

	// Linked with a stored PIN: the same PIN is sent again.
	st.rows["urbanloft"] = map[string]string{"phone_number_id": urbanPhone, "waba_id": portfolioWABA, "token_source": "platform", "registration_pin": "777777"}
	sim.numbers[urbanPhone] = &simNumber{status: "DISCONNECTED", platformType: "CLOUD_API", pin: "777777"}
	w = httptest.NewRecorder()
	h.Reregister(w, ownerRequest(http.MethodPost, "/x", "urbanloft", ""))
	mustOK(t, "reregister", w)
	if n := sim.number(urbanPhone); n.status != "CONNECTED" || n.pin != "777777" {
		t.Fatalf("number %+v", n)
	}
}

func TestWebsocketOriginPatterns(t *testing.T) {
	got := wsOriginPatterns([]string{"https://notifications.codevertexafrica.com", " http://localhost:3000/ "})
	want := map[string]bool{"notifications.codevertexafrica.com": true, "localhost:3000": true, "codevertexafrica.com": true, "*.codevertexafrica.com": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("unexpected pattern %q in %v", p, got)
		}
	}
	if p := wsOriginPatterns(nil); len(p) != 1 || p[0] != "*" {
		t.Fatalf("no configured origins: %v", p)
	}
}

// The inbox socket accepts the notifications UI's origin on the API host (a different host), and
// still refuses an origin that is not configured.
func TestInboxWebsocketAcceptsUIOrigin(t *testing.T) {
	patterns := wsOriginPatterns([]string{"https://notifications.codevertexafrica.com"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: patterns})
		if err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	for origin, ok := range map[string]bool{
		"https://notifications.codevertexafrica.com": true,
		"https://evil.example.com":                   false,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
		cancel()
		if (err == nil) != ok {
			t.Errorf("%s: dial err %v, want accepted=%v", origin, err, ok)
		}
		if c != nil {
			_ = c.Close(websocket.StatusNormalClosure, "")
		}
	}
}

func codeStatus(n *simNumber) string {
	if n.needsVerification {
		return "EXPIRED"
	}
	return "VERIFIED"
}

func ownerJSON(h http.HandlerFunc, path, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, ownerRequest(http.MethodPost, path, target, body))
	return w
}

// A number deregistered for a while needs re-verification (133006). Re-register says so with
// needs_verification; a code is requested, a wrong code is refused with a hint, a resend replaces
// the code, and the right code verifies and registers the number. Works for a portfolio tenant
// and for the platform tenant (All Tenants).
func TestVerifyThenRegister(t *testing.T) {
	for _, tc := range []struct{ target, tenant, phone string }{
		{"urbanloft", "urbanloft", urbanPhone},
		{"", "platform-tenant", platformPhone},
	} {
		sim := newMetaSim(map[string][]string{portfolioWABA: {platformPhone, urbanPhone}})
		srv := sim.server(t)
		h, st := newSimHandler(t, srv.URL)
		clock := time.Date(2026, 11, 15, 15, 0, 0, 0, time.UTC)
		h.now = func() time.Time { return clock }
		st.rows[tc.tenant] = map[string]string{"phone_number_id": tc.phone, "waba_id": portfolioWABA, "token_source": "platform", "registration_pin": "777777"}
		sim.numbers[tc.phone] = &simNumber{status: "DISCONNECTED", platformType: "CLOUD_API", pin: "777777", needsVerification: true}
		sim.subscribed[portfolioWABA] = true

		w := ownerJSON(h.Reregister, "/reregister", tc.target, "")
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusUnprocessableEntity || body["needs_verification"] != true {
			t.Fatalf("%s: reregister status %d: %s", tc.tenant, w.Code, w.Body.String())
		}

		state := httptest.NewRecorder()
		h.RegistrationStatus(state, ownerRequest(http.MethodGet, "/registration", tc.target, ""))
		var st1 map[string]any
		_ = json.Unmarshal(state.Body.Bytes(), &st1)
		if st1["needs_verification"] != true || st1["registered"] != false {
			t.Fatalf("%s: registration state %v", tc.tenant, st1)
		}

		mustOK(t, "request", ownerJSON(h.RequestVerificationCode, "/verification/request", tc.target, `{"code_method":"SMS"}`))
		// A second request straight away is refused by our rate limit, not sent to Meta.
		if w := ownerJSON(h.RequestVerificationCode, "/verification/request", tc.target, `{"code_method":"VOICE"}`); w.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: immediate resend status %d", tc.tenant, w.Code)
		}

		w = ownerJSON(h.VerifyCode, "/verification/verify", tc.target, `{"code":"000000"}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "wrong or has expired") || strings.Contains(w.Body.String(), "000000") {
			t.Fatalf("%s: wrong code status %d: %s", tc.tenant, w.Code, w.Body.String())
		}

		clock = clock.Add(61 * time.Second)
		mustOK(t, "resend", ownerJSON(h.RequestVerificationCode, "/verification/request", tc.target, `{"code_method":"VOICE"}`))
		if n := sim.number(tc.phone); n.sent != 2 || strings.Join(n.methods, ",") != "SMS,VOICE" {
			t.Fatalf("%s: codes sent %d by %v", tc.tenant, n.sent, n.methods)
		}
		// The first code was replaced by the resend.
		if w := ownerJSON(h.VerifyCode, "/verification/verify", tc.target, `{"code":"481516"}`); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: old code status %d", tc.tenant, w.Code)
		}
		w = ownerJSON(h.VerifyCode, "/verification/verify", tc.target, `{"code":"234 200"}`)
		mustOK(t, "verify", w)
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body["registered"] != true || body["status"] != "CONNECTED" || body["code_verification_status"] != "VERIFIED" {
			t.Fatalf("%s: verify answer %v", tc.tenant, body)
		}
		if n := sim.number(tc.phone); n.pin != "777777" {
			t.Fatalf("%s: registered with PIN %q, want the stored one", tc.tenant, n.pin)
		}
		sendReady(t, h, sim, tc.tenant)
	}
}

func TestVerificationInputAndAccess(t *testing.T) {
	sim := newMetaSim(map[string][]string{portfolioWABA: {urbanPhone}})
	srv := sim.server(t)
	h, st := newSimHandler(t, srv.URL)
	st.rows["urbanloft"] = map[string]string{"phone_number_id": urbanPhone, "waba_id": portfolioWABA, "token_source": "platform"}
	for _, body := range []string{`{"code_method":"EMAIL"}`, `{"code_method":"SMS","language":"en-US; drop"}`} {
		if w := ownerJSON(h.RequestVerificationCode, "/r", "urbanloft", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", body, w.Code)
		}
	}
	if w := ownerJSON(h.VerifyCode, "/v", "urbanloft", `{"code":"12ab"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad code status %d", w.Code)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/r", bytes.NewBufferString(`{"code_method":"SMS"}`))
	h.RequestVerificationCode(w, r.WithContext(httpware.WithTenantID(r.Context(), "urbanloft")))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non owner status %d", w.Code)
	}
	if n := sim.number(urbanPhone); n.sent != 0 {
		t.Fatal("a refused request reached Meta")
	}
}

// Link answers needs_verification too, so the panel opens the verification step.
func TestManagedLinkNeedsVerification(t *testing.T) {
	sim := newMetaSim(map[string][]string{portfolioWABA: {urbanPhone}})
	sim.numbers[urbanPhone] = &simNumber{status: "DISCONNECTED", platformType: "CLOUD_API", needsVerification: true}
	srv := sim.server(t)
	h, _ := newSimHandler(t, srv.URL)
	w := simLink(h, "urbanloft", urbanPhone, portfolioWABA)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"needs_verification":true`) {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	// The link is saved, so the number is verified and registered in place.
	if p, _, _, _ := h.conn(context.Background(), "urbanloft"); p != urbanPhone {
		t.Fatalf("link not saved: %q", p)
	}
	mustOK(t, "request", ownerJSON(h.RequestVerificationCode, "/r", "urbanloft", `{"code_method":"SMS"}`))
	mustOK(t, "verify", ownerJSON(h.VerifyCode, "/v", "urbanloft", `{"code":"481516"}`))
	sendReady(t, h, sim, "urbanloft")
}
