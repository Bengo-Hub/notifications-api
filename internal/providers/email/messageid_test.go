package email

import (
	"context"
	"testing"
)

func TestMessageIDToken(t *testing.T) {
	if got := MessageIDToken("<7f3c1e2a-9b1d-4c1a-8d52-0a1b2c3d4e5f@codevertexafrica.com>"); got != "7f3c1e2a-9b1d-4c1a-8d52-0a1b2c3d4e5f" {
		t.Fatalf("token %q", got)
	}
	for _, generated := range []string{"<1760947200000000000.ab12cd34@codevertexafrica.com>", "no-at-sign", "", "<@x.com>"} {
		if got := MessageIDToken(generated); got != "" {
			t.Fatalf("%q must carry no token, got %q", generated, got)
		}
	}
}

func TestWithMessageIDRejectsUnsafeTokens(t *testing.T) {
	ctx := WithMessageID(context.Background(), "abc-123")
	if messageIDToken(ctx) != "abc-123" {
		t.Fatal("a plain token must be kept")
	}
	for _, bad := range []string{"", "  ", "a@b", "a b", "<a>", "a\r\nBcc: x@y.com"} {
		if got := messageIDToken(WithMessageID(context.Background(), bad)); got != "" {
			t.Fatalf("%q must be ignored (header injection guard), got %q", bad, got)
		}
	}
}
