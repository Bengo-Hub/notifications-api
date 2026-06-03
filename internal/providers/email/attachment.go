package email

// Attachment is an optional file attached to an outgoing email. Providers include
// attachments only when the slice passed to SendEmail is non-empty.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}
