package email

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const testDomain = "codevertexafrica.com"

// threadHeaderText is what the SMTP sender writes for ctx and its own Message-ID.
func threadHeaderText(ctx context.Context, own string) string {
	var b strings.Builder
	writeThreadHeaders(ctx, &b, own, testDomain)
	return b.String()
}

// An agent reply answers the customer's last message; References starts at the ticket's root.
func TestThreadHeadersReferencesOrder(t *testing.T) {
	ctx := WithMessageID(context.Background(), "comment-1")
	ctx = WithThreadHeaders(ctx, "ticket-1", "<cust-2@mail.example.com>",
		[]string{"<cust-1@mail.example.com>", "<cust-2@mail.example.com>"})
	got := threadHeaderText(ctx, "<comment-1@"+testDomain+">")
	want := "In-Reply-To: <cust-2@mail.example.com>\r\n" +
		"References: <ticket-1@codevertexafrica.com>\r\n <cust-1@mail.example.com>\r\n <cust-2@mail.example.com>\r\n"
	if got != want {
		t.Fatalf("headers\n%q\nwant\n%q", got, want)
	}
}

// The ticket's first receipt is the thread's root, so it never references itself.
func TestThreadHeadersSkipOwnRoot(t *testing.T) {
	own := "<ticket-1@" + testDomain + ">"
	// A ticket raised by email: the receipt answers the customer's message.
	ctx := WithThreadHeaders(context.Background(), "ticket-1", "<cust-1@mail.example.com>", []string{"<cust-1@mail.example.com>"})
	if got, want := threadHeaderText(ctx, own), "In-Reply-To: <cust-1@mail.example.com>\r\nReferences: <cust-1@mail.example.com>\r\n"; got != want {
		t.Fatalf("email ticket receipt %q, want %q", got, want)
	}
	// A ticket raised on the web: the receipt starts the thread and needs neither header.
	ctx = WithThreadHeaders(context.Background(), "ticket-1", "", nil)
	if got := threadHeaderText(ctx, own); got != "" {
		t.Fatalf("web ticket receipt must not reference itself: %q", got)
	}
	// A later status email on a web ticket answers the receipt.
	ctx = WithThreadHeaders(context.Background(), "ticket-1", "", nil)
	if got, want := threadHeaderText(ctx, "<status-1@"+testDomain+">"), "In-Reply-To: "+own+"\r\nReferences: "+own+"\r\n"; got != want {
		t.Fatalf("status email %q, want %q", got, want)
	}
}

func TestThreadHeadersRefuseInjection(t *testing.T) {
	bad := []string{
		"<a@b.com>\r\nBcc: x@y.com",
		"<a@b.com>\nBcc: x@y.com",
		"a@b.com",
		"<a b@c.com>",
		"<a@b@c.com>",
		"<@b.com>",
		"<a@>",
		"<a@b.com>, <c@d.com>",
		"<\"a\"@b.com>",
	}
	ctx := WithThreadHeaders(context.Background(), "t\r\nBcc: x@y.com", bad[0], bad)
	if _, ok := ctx.Value(threadKey{}).(threadHeaders); ok {
		t.Fatal("a context with only unsafe values must carry no thread")
	}
	if got := threadHeaderText(ctx, "<own@"+testDomain+">"); got != "" {
		t.Fatalf("unsafe values were written: %q", got)
	}
	// Good ids survive next to bad ones, and nothing written holds a bare CR or LF.
	ctx = WithThreadHeaders(context.Background(), "", "<ok@mail.example.com>", append([]string{"<first@mail.example.com>"}, bad...))
	got := threadHeaderText(ctx, "<own@"+testDomain+">")
	if !strings.Contains(got, "<first@mail.example.com>") || strings.Contains(got, "Bcc") {
		t.Fatalf("mixed values %q", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n") {
		if strings.ContainsAny(line, "\r\n") || !(strings.HasPrefix(line, "In-Reply-To: ") || strings.HasPrefix(line, "References: ") || strings.HasPrefix(line, " <")) {
			t.Fatalf("unexpected header line %q in %q", line, got)
		}
	}
}

func TestThreadHeadersAbsentWithoutThread(t *testing.T) {
	ctx := WithMessageID(context.Background(), "req-1")
	if got := threadHeaderText(ctx, "<req-1@"+testDomain+">"); got != "" {
		t.Fatalf("no thread fields must write no header: %q", got)
	}
	if h := brevoThreadHeaders(ctx, "Kura <support@kura.example>"); h != nil {
		t.Fatalf("no thread fields must send no Brevo header: %v", h)
	}
	if WithThreadHeaders(ctx, "", "", nil) != ctx {
		t.Fatal("an empty thread must leave the context unchanged")
	}
}

// Brevo gets the same values, rooted at the sender's domain, with the root skipped on the receipt.
func TestBrevoThreadHeaders(t *testing.T) {
	ctx := WithMessageID(context.Background(), "comment-1")
	ctx = WithThreadHeaders(ctx, "ticket-1", "<cust-1@mail.example.com>", []string{"<cust-1@mail.example.com>"})
	h := brevoThreadHeaders(ctx, "Kura <support@kura.example>")
	if h["In-Reply-To"] != "<cust-1@mail.example.com>" || h["References"] != "<ticket-1@kura.example> <cust-1@mail.example.com>" {
		t.Fatalf("brevo headers %v", h)
	}
	receipt := WithThreadHeaders(WithMessageID(context.Background(), "ticket-1"), "ticket-1", "", nil)
	if h := brevoThreadHeaders(receipt, "support@kura.example"); h != nil {
		t.Fatalf("the receipt must not reference itself on Brevo: %v", h)
	}
}

// A long thread keeps its root and the most recent ids.
func TestThreadHeadersCapKeepsRoot(t *testing.T) {
	var refs []string
	for i := 0; i < 40; i++ {
		refs = append(refs, fmt.Sprintf("<m%d@mail.example.com>", i))
	}
	ctx := WithThreadHeaders(context.Background(), "ticket-1", refs[39], refs)
	_, got := threadHeaderValues(ctx, "<own@"+testDomain+">", testDomain)
	if len(got) != maxThreadReferences || got[0] != "<ticket-1@"+testDomain+">" || got[len(got)-1] != refs[39] {
		t.Fatalf("capped references %v", got)
	}
}
