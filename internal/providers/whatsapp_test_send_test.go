package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeWA struct {
	calls  []map[string]interface{}
	bodies []string
	err    error
}

func (f *fakeWA) SendWhatsApp(_ context.Context, _ string, _ []string, body string, md map[string]interface{}) error {
	f.calls = append(f.calls, md)
	f.bodies = append(f.bodies, body)
	return f.err
}
func (f *fakeWA) Name() string { return "fake" }

// The test is a free plain text, never a (paid) template.
func TestWhatsAppTestSendIsPlainText(t *testing.T) {
	f := &fakeWA{}
	if err := whatsAppTestSend(context.Background(), f, "254700000000"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0]["template_name"] != nil || f.bodies[0] == "" {
		t.Fatalf("calls = %v bodies = %q", f.calls, f.bodies)
	}
}

// Outside the 24 hour window (#131047) the error says what to do.
func TestWhatsAppTestSendExplainsTheWindow(t *testing.T) {
	f := &fakeWA{err: errors.New(`(#131047) Re-engagement message`)}
	err := whatsAppTestSend(context.Background(), f, "254700000000")
	if err == nil || !strings.Contains(err.Error(), "24 hours") || !strings.Contains(err.Error(), "254700000000") {
		t.Fatalf("got %v", err)
	}
}

// Any other Meta error is returned unchanged.
func TestWhatsAppTestSendOtherErrorsPassThrough(t *testing.T) {
	f := &fakeWA{err: errors.New("(#100) Invalid parameter")}
	err := whatsAppTestSend(context.Background(), f, "254700000000")
	if err == nil || !strings.Contains(err.Error(), "#100") {
		t.Fatalf("got %v", err)
	}
}
