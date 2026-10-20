package email

import (
	"context"
	"strings"
)

type messageIDKey struct{}

// WithMessageID makes a send use the given token as the local part of its Message-ID, so the id
// the mail server reports in delivery events can be matched back to the delivery log row that
// holds the same token (the notification request id). Without it the provider generates a random one.
func WithMessageID(ctx context.Context, token string) context.Context {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "<>@ \r\n") {
		return ctx
	}
	return context.WithValue(ctx, messageIDKey{}, token)
}

// messageIDToken returns the token set by WithMessageID, or "".
func messageIDToken(ctx context.Context) string {
	t, _ := ctx.Value(messageIDKey{}).(string)
	return t
}

// MessageIDToken extracts the request token from a Message-ID such as "<token@domain>", the
// inverse of what WithMessageID produces. It returns "" for ids the provider generated itself.
func MessageIDToken(messageID string) string {
	id := strings.Trim(strings.TrimSpace(messageID), "<>")
	at := strings.LastIndex(id, "@")
	if at <= 0 {
		return ""
	}
	local := id[:at]
	if strings.Contains(local, ".") {
		return "" // generated ids look like "<nanos.hex@domain>"
	}
	return local
}
