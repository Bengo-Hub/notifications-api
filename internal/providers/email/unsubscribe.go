package email

import "context"

type listUnsubscribeKey struct{}

// WithListUnsubscribe attaches a one-click unsubscribe URL (https) to a send. Providers then set
// List-Unsubscribe with that URL and List-Unsubscribe-Post: List-Unsubscribe=One-Click (RFC 8058),
// which Gmail and Yahoo require for bulk mail. Marketing broadcasts set it; transactional mail
// does not.
func WithListUnsubscribe(ctx context.Context, url string) context.Context {
	if url == "" {
		return ctx
	}
	return context.WithValue(ctx, listUnsubscribeKey{}, url)
}

// ListUnsubscribeURL returns the URL set by WithListUnsubscribe, or "".
func ListUnsubscribeURL(ctx context.Context) string {
	u, _ := ctx.Value(listUnsubscribeKey{}).(string)
	return u
}
