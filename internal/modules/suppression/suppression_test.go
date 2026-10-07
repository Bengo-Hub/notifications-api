package suppression

import (
	"testing"

	"github.com/google/uuid"
)

func TestTokenRoundTripAndTamper(t *testing.T) {
	s := NewService(nil, []byte("0123456789abcdef0123456789abcdef"))
	tid := uuid.New()
	tok := s.TokenFor(&tid, "email", "Titus@Example.com", "Codevertex Africa")
	got, err := s.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != tid.String() || got.Channel != "email" || got.Sender != "Codevertex Africa" || got.AddressHash == "" {
		t.Fatalf("round trip: %+v", got)
	}
	if got.AddressHash == "titus@example.com" {
		t.Fatal("the token must not carry the address")
	}
	if _, err := s.Verify(tok[:len(tok)-2] + "xx"); err == nil {
		t.Error("a changed signature must fail")
	}
	other := NewService(nil, []byte("another-key-another-key-another-k"))
	if _, err := other.Verify(tok); err == nil {
		t.Error("a token from another key must fail")
	}
	rotated := NewService(nil, []byte("another-key-another-key-another-k"), []byte("0123456789abcdef0123456789abcdef"))
	if _, err := rotated.Verify(tok); err != nil {
		t.Error("a link signed before a key rotation must still verify")
	}
	if _, err := s.Verify("nonsense"); err == nil {
		t.Error("garbage must fail")
	}
	plat := s.TokenFor(nil, "sms", "+254712345678", "")
	if p, err := s.Verify(plat); err != nil || p.TenantID != "" {
		t.Errorf("platform token: %+v %v", p, err)
	}
}

func TestIsStopReply(t *testing.T) {
	for _, s := range []string{"STOP", "stop", " Stop. ", "UNSUBSCRIBE", "Stop promotions", "acha", "Sitisha!"} {
		if !IsStopReply(s) {
			t.Errorf("%q should stop", s)
		}
	}
	for _, s := range []string{"please stop by tomorrow", "stopped", "", "Thanks"} {
		if IsStopReply(s) {
			t.Errorf("%q should not stop", s)
		}
	}
}
