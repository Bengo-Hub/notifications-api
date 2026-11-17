package email

import (
	"context"
	"strings"
)

type threadKey struct{}

// threadHeaders is what WithThreadHeaders stores for the sender.
type threadHeaders struct {
	rootToken  string   // local part of the thread's first message, e.g. a ticket id
	inReplyTo  string   // the message this one answers, "<id@domain>"
	references []string // the thread's earlier Message-IDs, oldest first, "<id@domain>"
}

// maxThreadReferences bounds the References header on a long thread. The first entry (the
// thread's root) is always kept, with the most recent ones after it, as RFC 5322 suggests.
const maxThreadReferences = 20

// maxMessageIDLen bounds one Message-ID; anything longer is not a real id.
const maxMessageIDLen = 250

// IsMessageIDToken reports whether token can be the local part of a Message-ID this service
// writes: not empty, and free of the characters that would break the id or the header.
func IsMessageIDToken(token string) bool {
	token = strings.TrimSpace(token)
	return token != "" && !strings.ContainsAny(token, "<>@ \t\r\n")
}

// WithThreadHeaders makes a send answer an earlier email: In-Reply-To names inReplyTo and
// References lists "<threadToken@sending domain>" followed by references, so the reader's mail
// client threads the messages and a reply carries ids the sending service can recognise.
// Values that are not a well formed Message-ID are dropped, so nothing from an event can add a
// header. With nothing usable the context is returned unchanged and neither header is written.
func WithThreadHeaders(ctx context.Context, threadToken, inReplyTo string, references []string) context.Context {
	var th threadHeaders
	if IsMessageIDToken(threadToken) {
		th.rootToken = strings.TrimSpace(threadToken)
	}
	if id, ok := cleanMessageID(inReplyTo); ok {
		th.inReplyTo = id
	}
	for _, r := range references {
		if id, ok := cleanMessageID(r); ok {
			th.references = append(th.references, id)
		}
	}
	if th.rootToken == "" && th.inReplyTo == "" && len(th.references) == 0 {
		return ctx
	}
	return context.WithValue(ctx, threadKey{}, th)
}

// cleanMessageID returns s trimmed when it is one Message-ID in angle brackets, "<left@right>",
// made only of the printable characters RFC 5322 allows in an id.
func cleanMessageID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 5 || len(s) > maxMessageIDLen || s[0] != '<' || s[len(s)-1] != '>' {
		return "", false
	}
	inner := s[1 : len(s)-1]
	at := strings.IndexByte(inner, '@')
	if at <= 0 || at == len(inner)-1 || strings.Count(inner, "@") != 1 {
		return "", false
	}
	for i := 0; i < len(inner); i++ {
		if !isMessageIDChar(inner[i]) {
			return "", false
		}
	}
	return s, true
}

// isMessageIDChar is an RFC 5322 atext character, a dot, the at sign or a domain literal bracket.
// Whitespace, control characters (CR and LF among them), quotes, commas and angle brackets fail.
func isMessageIDChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-/=?^_`{|}~.@[]", c) >= 0
}

// threadHeaderValues is the In-Reply-To and References for a message whose own Message-ID is
// ownMessageID, sent from domain. References starts with the thread's root, "<root@domain>",
// then the earlier ids in order, without repeats and without the message's own id (the first
// email of a thread is its own root). In-Reply-To is the given parent, or else the last entry
// of References. Both are empty when the context carries no thread.
func threadHeaderValues(ctx context.Context, ownMessageID, domain string) (string, []string) {
	th, ok := ctx.Value(threadKey{}).(threadHeaders)
	if !ok {
		return "", nil
	}
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(ownMessageID)): true}
	var refs []string
	add := func(id string) {
		key := strings.ToLower(id)
		if id == "" || seen[key] {
			return
		}
		seen[key] = true
		refs = append(refs, id)
	}
	if th.rootToken != "" && domain != "" {
		if id, ok := cleanMessageID("<" + th.rootToken + "@" + domain + ">"); ok {
			add(id)
		}
	}
	for _, r := range th.references {
		add(r)
	}
	// The parent belongs at the end of References when the list did not already carry it.
	add(th.inReplyTo)
	if len(refs) > maxThreadReferences {
		recent := refs[len(refs)-(maxThreadReferences-1):]
		refs = append([]string{refs[0]}, recent...)
	}

	inReplyTo := th.inReplyTo
	if strings.EqualFold(inReplyTo, strings.TrimSpace(ownMessageID)) {
		inReplyTo = ""
	}
	if inReplyTo == "" && len(refs) > 0 {
		inReplyTo = refs[len(refs)-1]
	}
	return inReplyTo, refs
}

// domainOf is the domain part of an address such as "Name <user@example.com>", or "".
func domainOf(addr string) string {
	e := extractEmail(addr)
	if at := strings.LastIndex(e, "@"); at >= 0 && at < len(e)-1 {
		return e[at+1:]
	}
	return ""
}
