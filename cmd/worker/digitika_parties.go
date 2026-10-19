package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Digitika (education) notices reach the learner and, when there is a parent or guardian, them
// too. A child often has no email or phone of their own, so each person is reached on whatever
// contact they have; sendToParties (notify_parties.go) skips a channel with no contact. The
// learner's and guardian's mail and WhatsApp are built here once for every Digitika notice.

// digitikaButtonBase is the fixed domain of every Digitika WhatsApp URL button (Meta fixes it at
// approval); only the path after it varies per message.
const digitikaButtonBase = "https://codevertexafrica.com/"

// digitikaPeople are the people a Digitika notice is for.
type digitikaPeople struct {
	StudentID     string
	StudentName   string
	StudentEmail  string // real address only; a child without one has none
	StudentPhone  string
	GuardianName  string
	GuardianEmail string
	GuardianPhone string
}

// peopleFromPayload reads the learner and guardian contacts the website puts on its events. A
// placeholder student email (a child with no address of their own) is dropped by sendableEmail
// when the party is sent, so it needs no special case here.
func peopleFromPayload(p map[string]any) digitikaPeople {
	return digitikaPeople{
		StudentID:     str(p, "studentId"),
		StudentName:   str(p, "studentName"),
		StudentEmail:  strings.TrimSpace(str(p, "studentEmail")),
		StudentPhone:  strings.TrimSpace(str(p, "studentPhone")),
		GuardianName:  strings.TrimSpace(str(p, "guardianName")),
		GuardianEmail: strings.TrimSpace(str(p, "guardianEmail")),
		GuardianPhone: strings.TrimSpace(str(p, "guardianPhone")),
	}
}

// recipient is one person a notice is worded for. The same notice reads "your place" to the
// learner and "Clara's place" to a parent.
type recipient struct {
	Name     string // who the greeting names
	Student  string // the learner's name
	Guardian bool
}

// Who is the subject of a sentence: "You" or the learner's name.
func (r recipient) Who() string {
	if r.Guardian {
		return r.Student
	}
	return "You"
}

// Whose is the possessive: "your" or "Clara's".
func (r recipient) Whose() string {
	if r.Guardian {
		return r.firstName() + "'s"
	}
	return "your"
}

// Place is the subject for "place is confirmed": "Your place" or "Clara's place".
func (r recipient) Place() string {
	if !r.Guardian {
		return "Your place"
	}
	return r.firstName() + "'s place"
}

func (r recipient) firstName() string {
	if f := strings.Fields(r.Student); len(f) > 0 {
		return f[0]
	}
	return "the learner"
}

// guardianFor is the learner's name for a guardian's email text, empty for the learner so the
// templates can tell the two apart.
func (r recipient) guardianFor() string {
	if r.Guardian {
		return r.Student
	}
	return ""
}

// whatsAppFor builds the approved WhatsApp template for one recipient (nil: no WhatsApp).
type whatsAppFor func(r recipient) *partyWhatsApp

// parties lists the learner and the guardian as the notice's parties. The learner is always
// listed; the guardian only when they have a contact that is not already the learner's, so one
// phone shared by parent and child gets one message. Contacts a party lacks are skipped by
// sendToParties, never invented.
func (d digitikaPeople) parties(target string, wa whatsAppFor) []noticeParty {
	learner := recipient{Name: d.StudentName, Student: d.StudentName}
	out := []noticeParty{d.party("student", target, d.StudentEmail, d.StudentPhone, learner, wa)}

	if d.GuardianEmail == "" && d.GuardianPhone == "" {
		return out
	}
	email, phone := d.GuardianEmail, d.GuardianPhone
	if strings.EqualFold(email, d.StudentEmail) {
		email = ""
	}
	if phone != "" && phone == d.StudentPhone {
		phone = ""
	}
	if email == "" && phone == "" {
		return out
	}
	name := d.GuardianName
	if name == "" {
		name = "there"
	}
	guardian := recipient{Name: name, Student: d.StudentName, Guardian: true}
	return append(out, d.party("guardian", target, email, phone, guardian, wa))
}

func (d digitikaPeople) party(key, target, email, phone string, r recipient, wa whatsAppFor) noticeParty {
	p := noticeParty{
		Key:    key,
		Target: target,
		Email:  email,
		Phone:  phone,
		// name greets the recipient; for_student is set only for a guardian ("for Clara").
		Data: map[string]any{"name": r.Name, "for_student": r.guardianFor()},
	}
	if wa != nil {
		p.WhatsApp = wa(r)
	}
	return p
}

// digitikaButton is the URL suffix of a Digitika link, or "" when the link is not on the
// Digitika site (a WhatsApp button template cannot go without it).
func digitikaButton(link string) string {
	link = strings.TrimSpace(link)
	if !strings.HasPrefix(link, digitikaButtonBase) {
		return ""
	}
	return strings.TrimPrefix(link, digitikaButtonBase)
}

// ── payload readers ──────────────────────────────────────────────────────────

func str(p map[string]any, key string) string {
	v, _ := p[key].(string)
	return v
}

func num(p map[string]any, key string) float64 {
	switch n := p[key].(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

// amountText is "KES 8,000" for a WhatsApp or email value.
func amountText(amount float64, currency string) string {
	return formatMoney(amount, currency)
}

// prettyDate turns 2026-11-15 into "15 Nov 2026", leaving anything else as it was.
func prettyDate(iso string) string {
	if t, err := time.Parse("2006-01-02", strings.TrimSpace(iso)); err == nil {
		return t.Format("2 Jan 2006")
	}
	return iso
}

// ordinalOf is the "2nd of 3" label of an installment.
func ordinalOf(no, total int) string {
	if total > 1 {
		return fmt.Sprintf("%s of %d", ordinalLabel(no), total)
	}
	return ordinalLabel(no)
}

// partyOf builds a one-recipient notice party (account owners, contact form senders, one-time
// codes): the person, not a learner and guardian pair.
func partyOf(target, email, phone string, data map[string]any, wa *partyWhatsApp) noticeParty {
	return noticeParty{Target: target, Email: email, Phone: phone, Data: data, WhatsApp: wa}
}
