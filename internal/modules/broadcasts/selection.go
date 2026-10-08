package broadcasts

import (
	"context"

	"github.com/Bengo-Hub/httpware/contact"
	"github.com/Bengo-Hub/httpware/pii"

	"github.com/bengobox/notifications-api/internal/ent"
	entbroadcast "github.com/bengobox/notifications-api/internal/ent/broadcast"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
)

// candidate is one person's chosen address on one channel.
type candidate struct {
	person     Person
	channel    string
	address    string
	first      string
	backups    []string
	reason     string // why nothing will be sent ("" = it will be)
	suppressed bool   // the address opted out
	excluded   bool   // the sender left this person out
}

// sendable is true when this candidate will actually be sent to.
func (c candidate) sendable() bool {
	return c.address != "" && !c.suppressed && !c.excluded && c.reason == ""
}

// excludedKeys are the people the sender unticked in the recipient review (metadata.excluded).
func excludedKeys(b *ent.Broadcast) map[string]bool {
	out := map[string]bool{}
	for _, k := range stringList(b.Metadata["excluded"]) {
		out[k] = true
	}
	return out
}

// selectCandidates is the one rule for who gets a broadcast on which address, used by the
// recipient review and the real send alike so what the sender sees is what goes out. Per person
// it takes the first valid email and phone in the resolver's order (owners and verified admins
// first), then picks channels from what is valid (personChannels). Someone with neither a valid
// email nor a valid phone is not in the list at all. Marketing consent and the sender's
// exclusions are applied on top.
func selectCandidates(b *ent.Broadcast, people []Person, channels []string) []candidate {
	marketing := b.Class == entbroadcast.ClassMarketing
	attested, _ := b.Metadata["consent_attested"].(bool)
	excluded := excludedKeys(b)
	var cands []candidate
	for _, p := range people {
		email, emailFirst := firstValidEmail(p)
		phones, phoneFirst := validPhones(p)
		for _, ch := range personChannels(channels, email != "", len(phones) > 0) {
			c := candidate{person: p, channel: ch, excluded: excluded[p.Key]}
			consent := p.SMSConsent
			if ch == "email" {
				consent = p.EmailConsent
				c.address, c.first = email, emailFirst
			} else {
				c.address, c.first, c.backups = phones[0], phoneFirst, phones[1:]
			}
			if reason := marketingBlocked(consent, p.ConsentRecorded, attested); marketing && reason != "" {
				c.address, c.backups, c.reason = "", nil, reason
			}
			cands = append(cands, c)
		}
	}
	return cands
}

// personChannels picks which of the broadcast's channels one person gets: every channel when both
// a valid email and a valid phone are on file, email alone when only the email is valid, and one
// phone channel (WhatsApp, else SMS) when only the phone is valid. No valid address: none.
func personChannels(channels []string, hasEmail, hasPhone bool) []string {
	switch {
	case hasEmail && hasPhone:
		return channels
	case hasEmail:
		if contains(channels, "email") {
			return []string{"email"}
		}
	case hasPhone:
		for _, ch := range []string{"whatsapp", "sms"} {
			if contains(channels, ch) {
				return []string{ch}
			}
		}
	}
	return nil
}

// firstValidEmail is the person's first email that normalises, with the name behind it.
func firstValidEmail(p Person) (string, string) {
	for _, a := range p.Emails {
		if addr, err := contact.NormalizeEmail(a.Value); err == nil {
			return addr, a.FirstName
		}
	}
	return "", ""
}

// validPhones is every distinct valid number of the person in order (the first is used, the rest
// are backups), with the name behind the first.
func validPhones(p Person) ([]string, string) {
	var out []string
	first := ""
	seen := map[string]bool{}
	for _, a := range p.Phones {
		num, err := contact.NormalizePhone(a.Value, p.Region)
		if err != nil || seen[contact.SubscriberDigits(num)] {
			continue
		}
		seen[contact.SubscriberDigits(num)] = true
		if len(out) == 0 {
			first = a.FirstName
		}
		out = append(out, num)
	}
	return out, first
}

// markSuppressed flags candidates whose address opted out of this sender, one lookup per channel
// for the whole page.
func markSuppressed(ctx context.Context, supp *suppression.Service, b *ent.Broadcast, cands []candidate, channels []string) error {
	if supp == nil {
		return nil
	}
	marketing := b.Class == entbroadcast.ClassMarketing
	for _, ch := range channels {
		var hashes []string
		for _, c := range cands {
			if c.channel == ch && c.address != "" {
				hashes = append(hashes, pii.HashAddress(c.address))
			}
		}
		set, err := supp.Suppressed(ctx, b.TenantID, ch, hashes, marketing)
		if err != nil {
			return err
		}
		for i := range cands {
			if cands[i].channel == ch && cands[i].address != "" && set[pii.HashAddress(cands[i].address)] {
				cands[i].suppressed = true
			}
		}
	}
	return nil
}

// ReviewChannel is one channel of a person in the recipient review.
type ReviewChannel struct {
	Address string `json:"address"` // masked
	Sends   bool   `json:"sends"`
	Reason  string `json:"reason,omitempty"`
}

// ReviewRow is one person in the recipient review: who they are, whether the sender left them
// out, and per channel the (masked) address that would be used or why nothing would be sent.
type ReviewRow struct {
	Key          string                   `json:"key"`
	Name         string                   `json:"name"`
	BusinessName string                   `json:"business_name,omitempty"`
	Excluded     bool                     `json:"excluded"`
	Channels     map[string]ReviewChannel `json:"channels"`
}

// Review pages through a broadcast's audience as it would be sent now, for the sender to check
// and untick people before approving.
func Review(ctx context.Context, res Resolver, supp *suppression.Service, b *ent.Broadcast, after string, limit int) ([]ReviewRow, string, error) {
	channels := deliverableChannels(b.Channels)
	people, next, err := res.Page(ctx, b.Audience, b.TenantID, after, limit)
	if err != nil {
		return nil, "", err
	}
	cands := selectCandidates(b, people, channels)
	if err := markSuppressed(ctx, supp, b, cands, channels); err != nil {
		return nil, "", err
	}
	rows := make([]ReviewRow, 0, len(people))
	index := map[string]int{}
	for _, c := range cands {
		i, ok := index[c.person.Key]
		if !ok {
			i = len(rows)
			index[c.person.Key] = i
			rows = append(rows, ReviewRow{Key: c.person.Key, Name: firstNonEmpty(c.first, c.person.Name, c.person.BusinessName),
				BusinessName: c.person.BusinessName, Excluded: c.excluded, Channels: map[string]ReviewChannel{}})
		}
		rc := ReviewChannel{Sends: c.sendable(), Reason: c.reason}
		if c.address != "" {
			rc.Address = pii.Mask(c.address)
		}
		if c.suppressed {
			rc.Reason = "opted out"
		}
		if c.excluded && rc.Reason == "" {
			rc.Reason = "left out"
		}
		if rows[i].Name == "" || (c.first != "" && rows[i].Name == c.person.BusinessName) {
			rows[i].Name = firstNonEmpty(c.first, c.person.Name, c.person.BusinessName)
		}
		rows[i].Channels[c.channel] = rc
	}
	return rows, next, nil
}

// MaxExcluded caps how many people one broadcast can leave out by hand (kept in metadata); a
// larger cut is a segment, not a tick list.
const MaxExcluded = 5000
