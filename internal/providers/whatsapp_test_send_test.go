package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeWA answers each send with the next error in errs (nil once they run out).
type fakeWA struct {
	calls  []map[string]interface{}
	bodies []string
	errs   []error
}

func (f *fakeWA) SendWhatsApp(_ context.Context, _ string, _ []string, body string, md map[string]interface{}) error {
	f.calls = append(f.calls, md)
	f.bodies = append(f.bodies, body)
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}
func (f *fakeWA) Name() string { return "fake" }

// Owner, 14 November 2026: the free text test failed with #131047 outside the 24 hour window, so
// the test goes out as the approved utility template, which reaches any number.
func TestWhatsAppTestSendUsesTheTestTemplate(t *testing.T) {
	f := &fakeWA{}
	sentAs, err := whatsAppTestSend(context.Background(), f, "254700000000")
	if err != nil || sentAs != TestSentAsTemplate {
		t.Fatalf("sentAs = %q, err = %v", sentAs, err)
	}
	if len(f.calls) != 1 || f.calls[0]["template_name"] != WhatsAppTestTemplate {
		t.Fatalf("calls = %v", f.calls)
	}
}

// While the template is not approved on the tenant's account yet (#132001 and the other 1320xx
// template errors), the test falls back to the free text.
func TestWhatsAppTestSendFallsBackToText(t *testing.T) {
	f := &fakeWA{errs: []error{errors.New(`meta_cloud error: status 404: {"error":{"message":"(#132001) Template name does not exist in the translation","code":132001}}`)}}
	sentAs, err := whatsAppTestSend(context.Background(), f, "254700000000")
	if err != nil || sentAs != TestSentAsText {
		t.Fatalf("sentAs = %q, err = %v", sentAs, err)
	}
	if len(f.calls) != 2 || f.calls[1]["template_name"] != nil || f.bodies[1] == "" {
		t.Fatalf("calls = %v bodies = %q", f.calls, f.bodies)
	}
}

// Text fallback outside the 24 hour window (#131047): the error says what to do.
func TestWhatsAppTestSendExplainsTheWindow(t *testing.T) {
	f := &fakeWA{errs: []error{
		errors.New(`(#132001) Template name does not exist in the translation`),
		errors.New(`(#131047) Re-engagement message`),
	}}
	_, err := whatsAppTestSend(context.Background(), f, "254700000000")
	if err == nil || !strings.Contains(err.Error(), "24 hours") || !strings.Contains(err.Error(), "254700000000") ||
		!strings.Contains(err.Error(), "not approved") {
		t.Fatalf("got %v", err)
	}
}

// Any other Meta error on the template send (a malformed number, no payment method) is returned
// as it is, without a text retry that would hide it.
func TestWhatsAppTestSendOtherErrorsPassThrough(t *testing.T) {
	f := &fakeWA{errs: []error{errors.New("(#131009) Parameter value is not valid")}}
	_, err := whatsAppTestSend(context.Background(), f, "254700000000")
	if err == nil || !strings.Contains(err.Error(), "#131009") || len(f.calls) != 1 {
		t.Fatalf("got %v after %d calls", err, len(f.calls))
	}
}
