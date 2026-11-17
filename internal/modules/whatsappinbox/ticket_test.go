package whatsappinbox

import (
	"strings"
	"testing"
	"time"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ticketing"
)

// The ticket takes its subject from the first line, the whole message as its description, the
// conversation's customer as requester and the WhatsApp id as its key.
func TestTicketRequestFromAMessage(t *testing.T) {
	at := time.Date(2026, 11, 17, 8, 30, 0, 0, time.UTC)
	conv := &ent.WhatsAppConversation{CustomerName: " Amina Otieno ", CustomerWaID: "254712345678", PhoneNumberID: "pn-1"}
	msg := &ent.WhatsAppMessage{Body: "\n  My   order is late \nIt was due on Monday", WaMessageID: "wamid.1", CreatedAt: at}
	got := ticketRequest(conv, msg)
	want := ticketing.CreateRequest{
		Subject:        "My order is late",
		Description:    "My   order is late \nIt was due on Monday",
		RequesterName:  "Amina Otieno",
		RequesterPhone: "+254712345678",
		WAMessageID:    "wamid.1",
		PhoneNumberID:  "pn-1",
		ReceivedAt:     "2026-11-17T08:30:00Z",
	}
	if got != want {
		t.Fatalf("request\n got %+v\nwant %+v", got, want)
	}
}

func TestTicketSubjectIsBoundedAndFallsBackToTheSender(t *testing.T) {
	long := strings.Repeat("é", maxTicketSubject+10)
	if got := ticketSubject(long); len([]rune(got)) != maxTicketSubject {
		t.Fatalf("subject length %d", len([]rune(got)))
	}
	conv := &ent.WhatsAppConversation{CustomerWaID: "254700000000"}
	got := ticketRequest(conv, &ent.WhatsAppMessage{Body: "   ", WaMessageID: "w"})
	if got.Subject != "WhatsApp message from +254700000000" || got.Description != got.Subject {
		t.Fatalf("blank message: %+v", got)
	}
}
