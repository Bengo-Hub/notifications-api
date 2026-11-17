package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// threadPayload is a ticket event payload with ticketing-api's email thread fields.
func threadPayload(token string) map[string]any {
	return map[string]any{
		"ticket_id":           "0b5f8f9e-1111-4c1a-8d52-0a1b2c3d4e5f",
		"ticket_number":       "TKT-000042",
		"subject":             "Payout late",
		"requester_email":     "jane@example.com",
		"author_type":         "agent",
		"comment":             "We have sent it again.",
		"merged_into_number":  "TKT-000050",
		"email_message_token": token,
		"email_thread_token":  "0b5f8f9e-1111-4c1a-8d52-0a1b2c3d4e5f",
		"email_in_reply_to":   "<cust-2@mail.example.com>",
		"email_references":    []any{"<cust-1@mail.example.com>", "<cust-2@mail.example.com>"},
		"email_subject_tag":   "[TKT-000042]",
		"email_reply_subject": "Re: Payout late [TKT-000042]",
	}
}

func testTicketMessage(eventType string, p map[string]any) (string, map[string]any) {
	m := ticketingMappings[eventType]
	msg := ticketMessage(m, ticketingEvent{ID: "e1", TenantID: "t1", EventType: eventType, Payload: p}, []string{"jane@example.com"}, "customer", "", testTicketLinks)
	return msg.RequestID, msg.Metadata
}

// The receipt and an agent reply use ticketing's token as the request id (the Message-ID local
// part); events without one get a fresh id.
func TestTicketRequestIDFromToken(t *testing.T) {
	if id, _ := testTicketMessage("ticket.created", threadPayload("ticket-token")); id != "ticket-token" {
		t.Errorf("created request id %q", id)
	}
	if id, _ := testTicketMessage("ticket.comment_added", threadPayload("comment-token")); id != "comment-token" {
		t.Errorf("reply request id %q", id)
	}
	id, _ := testTicketMessage("ticket.resolved", threadPayload(""))
	if _, err := uuid.Parse(id); err != nil {
		t.Errorf("an event without a token gets a fresh uuid, got %q", id)
	}
	// A token that cannot be a Message-ID local part is not used.
	if id, _ := testTicketMessage("ticket.created", threadPayload("a@b")); id == "a@b" {
		t.Error("an unsafe token must not become the request id")
	}
}

func TestTicketSubjectChoice(t *testing.T) {
	cases := []struct {
		event, want string
	}{
		{"ticket.comment_added", "Re: Payout late [TKT-000042]"},
		{"ticket.created", "We have received your support request [TKT-000042]"},
		{"ticket.resolved", "Your ticket has been resolved [TKT-000042]"},
	}
	for _, c := range cases {
		if _, md := testTicketMessage(c.event, threadPayload("x")); md["subject"] != c.want {
			t.Errorf("%s subject %q, want %q", c.event, md["subject"], c.want)
		}
	}
	// The tag is not added twice when the subject already names it.
	p := threadPayload("x")
	p["email_reply_subject"] = ""
	m := ticketingMappings["ticket.comment_added"]
	m.EmailSubject = "Reply on [TKT-000042]"
	if s := ticketSubjectLine(m, p); s != "Reply on [TKT-000042]" {
		t.Errorf("tag repeated: %q", s)
	}
	// Staff alerts keep their own subject.
	if s := ticketSubjectLine(ticketingMappings["ticket.assigned"], threadPayload("x")); s != "Ticket assigned to you" {
		t.Errorf("agent subject %q", s)
	}
	// The customer's own words never break the subject onto a new header line.
	p = threadPayload("x")
	p["email_reply_subject"] = "Re: Payout\r\nBcc: x@y.com [TKT-000042]"
	if s := ticketSubjectLine(ticketingMappings["ticket.comment_added"], p); s != "Re: Payout Bcc: x@y.com [TKT-000042]" {
		t.Errorf("subject not folded: %q", s)
	}
}

// Requester mail carries the thread fields, through a JSON round trip as on NATS; staff alerts do not.
func TestTicketThreadMetadata(t *testing.T) {
	_, md := testTicketMessage("ticket.comment_added", threadPayload("c1"))
	raw, _ := json.Marshal(md)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if decoded[metaEmailThreadToken] != "0b5f8f9e-1111-4c1a-8d52-0a1b2c3d4e5f" || decoded[metaEmailInReplyTo] != "<cust-2@mail.example.com>" {
		t.Fatalf("thread metadata %v", decoded)
	}
	if refs := metadataStrings(decoded[metaEmailReferences]); !reflect.DeepEqual(refs, []string{"<cust-1@mail.example.com>", "<cust-2@mail.example.com>"}) {
		t.Fatalf("references %v", refs)
	}
	if ctx := withEmailThread(context.Background(), decoded); ctx == context.Background() {
		t.Fatal("thread fields must reach the send context")
	}

	_, staff := testTicketMessage("ticket.assigned", threadPayload("c1"))
	for _, k := range []string{metaEmailThreadToken, metaEmailInReplyTo, metaEmailReferences} {
		if _, ok := staff[k]; ok {
			t.Errorf("staff alert carries %s", k)
		}
	}
	// A message with no thread fields leaves the context as it was, so neither header is sent.
	bg := context.Background()
	if withEmailThread(bg, map[string]any{"subject": "Hello"}) != bg || withEmailThread(bg, nil) != bg {
		t.Error("a message without thread fields must not set thread headers")
	}
}
