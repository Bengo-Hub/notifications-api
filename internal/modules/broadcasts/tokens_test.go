package broadcasts

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	text := "Dear {first_name}, you are the reason we keep going the extra mile. Happy {occasion} from all of us at {sender_name}."
	got := Render(text, Vars{FirstName: "Titus", SenderName: "Codevertex Africa", Occasion: "Customer Service Week"})
	want := "Dear Titus, you are the reason we keep going the extra mile. Happy Customer Service Week from all of us at Codevertex Africa."
	if got != want {
		t.Fatalf("got %q", got)
	}
	if got := Render("Dear {first_name},", Vars{BusinessName: "Urban Loft"}); got != "Dear Urban Loft team," {
		t.Errorf("business fallback: %q", got)
	}
	if got := Render("Dear {first_name},", Vars{}); got != "Dear there," {
		t.Errorf("neutral fallback: %q", got)
	}
	if got := Render("Happy {year}!", Vars{Year: 2027}); got != "Happy 2027!" {
		t.Errorf("year: %q", got)
	}
	if got := Render("Literal {unknown} stays", Vars{}); got != "Literal {unknown} stays" {
		t.Errorf("unknown token must stay visible: %q", got)
	}
}

func TestUnknownTokensAndValidate(t *testing.T) {
	if u := UnknownTokens("Hi {firstname} and {first_name} {firstname}"); len(u) != 1 || u[0] != "firstname" {
		t.Fatalf("got %v", u)
	}
	if err := ValidateText(map[string]string{"email.body": "Hi {first_name}", "sms.body": "Hi {fname}"}); err == nil || !strings.Contains(err.Error(), "{fname} in sms.body") {
		t.Fatalf("expected error naming the field, got %v", err)
	}
	if err := ValidateText(map[string]string{"email.body": "Hi {first_name} from {sender_name}"}); err != nil {
		t.Fatal(err)
	}
}

func TestMarketingConsentRules(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name               string
		consent            *bool
		recorded, attested bool
		blocked            bool
	}{
		{"not tracked (tenants, staff)", nil, false, false, false},
		{"opted in with a record", &yes, true, false, false},
		{"opted in, older contact, no attestation", &yes, false, false, true},
		{"opted in, older contact, sender attests", &yes, false, true, false},
		{"opted out, even with an attestation", &no, true, true, true},
	}
	for _, c := range cases {
		if got := marketingBlocked(c.consent, c.recorded, c.attested) != ""; got != c.blocked {
			t.Errorf("%s: blocked=%v, want %v", c.name, got, c.blocked)
		}
	}
}

func TestWithSMSOptOut(t *testing.T) {
	if got := WithSMSOptOut("Happy New Year"); got != "Happy New Year\nReply STOP to opt out" {
		t.Errorf("got %q", got)
	}
	if got := WithSMSOptOut("Offer ends Friday. Reply STOP to unsubscribe"); strings.Count(got, "STOP") != 1 {
		t.Errorf("must not add a second opt-out: %q", got)
	}
}
