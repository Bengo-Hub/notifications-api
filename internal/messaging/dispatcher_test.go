package messaging

import "testing"

func TestMsgIDSeparatesChannelsRecipientsAndData(t *testing.T) {
	base := Message{IdempotencyKey: "treasury-invoice_sent-1", Channel: "email", TemplateID: "finance/invoice_sent", To: []string{"a@x.com"}, Data: map[string]any{"n": 1}}
	if msgID(base) != msgID(base) {
		t.Fatal("same message must give the same id")
	}
	other := base
	other.Channel = "whatsapp"
	if msgID(other) == msgID(base) {
		t.Error("same key on another channel must not collide")
	}
	other = base
	other.To = []string{"b@x.com"}
	if msgID(other) == msgID(base) {
		t.Error("same key to another recipient must not collide")
	}
	other = base
	other.Data = map[string]any{"n": 2}
	if msgID(other) == msgID(base) {
		t.Error("same key with new data (an OTP resend) must not collide")
	}
	if msgID(Message{Channel: "email"}) != "" {
		t.Error("no key means no dedup id")
	}
}

func TestNormalizeRecipientsKeepsWebPushTokenWhole(t *testing.T) {
	sub := `{"endpoint":"https://fcm.googleapis.com/fcm/send/abc","expirationTime":null,"keys":{"p256dh":"BH_x","auth":"MIM"}}`
	got := NormalizeRecipients([]string{sub, "fcm-token-1", sub}, "push")
	if len(got) != 2 || got[0] != sub || got[1] != "fcm-token-1" {
		t.Fatalf("push tokens must stay whole and deduped, got %q", got)
	}
	email := NormalizeRecipients([]string{"a@x.com, b@x.com"}, "email")
	if len(email) != 2 {
		t.Fatalf("email lists are still split, got %q", email)
	}
}
