package whatsapp

import (
	"context"
	"fmt"
	"strings"
)

// Interactive and media sends (MarketFlow Sprint 17). Like the cta_url message, these are
// free-form messages: Meta accepts them only inside the recipient's 24-hour reply window, so the
// caller decides when to use them and keeps templates for everything outside it.
//
// Metadata keys (all optional; template_name wins over every one of them):
//   - interactive_buttons: up to 3 reply buttons, [{"id": "...", "title": "..."}]. A tap comes back
//     on the webhook as interactive.button_reply with the same id.
//   - interactive_list_rows: up to 10 list rows, [{"id", "title", "description"}], opened by a
//     button labelled interactive_list_button (default "Choose"); interactive_list_section names
//     the one section. A tap comes back as interactive.list_reply.
//   - interactive_footer: small grey text under a button or list message.
//   - media_type ("image" or "video") with media_link: a native media message Meta fetches from
//     the link; the body is its caption.
//   - fallback_text: the plain text sent when Meta refuses an interactive message (it should still
//     tell the reader how to answer by typing). A refused media message falls back to its caption
//     with the link on its own line.
//
// Limits are Meta's: body 1,024 characters, button title 20, list button 20, row title 24, row
// description 72, row and button id 256, caption 1,024.
const (
	maxInteractiveBody   = 1024
	maxButtonTitle       = 20
	maxReplyButtons      = 3
	maxListRows          = 10
	maxListButton        = 20
	maxRowTitle          = 24
	maxRowDescription    = 72
	maxSectionTitle      = 24
	maxReplyID           = 256
	maxFooter            = 60
	maxCaption           = 1024
	defaultListButtonTxt = "Choose"
)

// ReplyButton is one reply button; ListRow is one row of a list message.
type ReplyButton struct {
	ID    string
	Title string
}

type ListRow struct {
	ID          string
	Title       string
	Description string
}

// clip cuts s to n runes.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n]))
	}
	return s
}

// objectList reads a metadata list of objects, as set by Go callers ([]map[string]string,
// []map[string]interface{}) or after a JSON round trip ([]interface{} of map[string]interface{}).
func objectList(v interface{}) []map[string]string {
	var out []map[string]string
	add := func(m map[string]interface{}) {
		row := map[string]string{}
		for k, val := range m {
			if s, ok := val.(string); ok {
				row[k] = s
			}
		}
		out = append(out, row)
	}
	switch raw := v.(type) {
	case []map[string]string:
		return raw
	case []map[string]interface{}:
		for _, m := range raw {
			add(m)
		}
	case []interface{}:
		for _, item := range raw {
			if m, ok := item.(map[string]interface{}); ok {
				add(m)
			} else if m, ok := item.(map[string]string); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// ReplyButtonsFrom reads metadata["interactive_buttons"]; buttons without an id or title are
// dropped and at most three are kept.
func ReplyButtonsFrom(metadata map[string]interface{}) []ReplyButton {
	var out []ReplyButton
	for _, m := range objectList(metadata["interactive_buttons"]) {
		b := ReplyButton{ID: clip(m["id"], maxReplyID), Title: clip(m["title"], maxButtonTitle)}
		if b.ID == "" || b.Title == "" {
			continue
		}
		out = append(out, b)
		if len(out) == maxReplyButtons {
			break
		}
	}
	return out
}

// ListRowsFrom reads metadata["interactive_list_rows"]; rows without an id or title are dropped
// and at most ten are kept.
func ListRowsFrom(metadata map[string]interface{}) []ListRow {
	var out []ListRow
	for _, m := range objectList(metadata["interactive_list_rows"]) {
		r := ListRow{ID: clip(m["id"], maxReplyID), Title: clip(m["title"], maxRowTitle), Description: clip(m["description"], maxRowDescription)}
		if r.ID == "" || r.Title == "" {
			continue
		}
		out = append(out, r)
		if len(out) == maxListRows {
			break
		}
	}
	return out
}

// ButtonsPayload builds a reply-button message.
func ButtonsPayload(to, body, footer string, buttons []ReplyButton) map[string]interface{} {
	btns := make([]map[string]interface{}, 0, len(buttons))
	for _, b := range buttons {
		btns = append(btns, map[string]interface{}{
			"type":  "reply",
			"reply": map[string]interface{}{"id": b.ID, "title": b.Title},
		})
	}
	interactive := map[string]interface{}{
		"type":   "button",
		"body":   map[string]interface{}{"text": clip(body, maxInteractiveBody)},
		"action": map[string]interface{}{"buttons": btns},
	}
	if f := clip(footer, maxFooter); f != "" {
		interactive["footer"] = map[string]interface{}{"text": f}
	}
	return map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "interactive",
		"interactive":       interactive,
	}
}

// ListPayload builds a list message with one section.
func ListPayload(to, body, footer, button, section string, rows []ListRow) map[string]interface{} {
	if button = clip(button, maxListButton); button == "" {
		button = defaultListButtonTxt
	}
	items := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		row := map[string]interface{}{"id": r.ID, "title": r.Title}
		if r.Description != "" {
			row["description"] = r.Description
		}
		items = append(items, row)
	}
	sec := map[string]interface{}{"rows": items}
	if s := clip(section, maxSectionTitle); s != "" {
		sec["title"] = s
	}
	interactive := map[string]interface{}{
		"type": "list",
		"body": map[string]interface{}{"text": clip(body, maxInteractiveBody)},
		"action": map[string]interface{}{
			"button":   button,
			"sections": []map[string]interface{}{sec},
		},
	}
	if f := clip(footer, maxFooter); f != "" {
		interactive["footer"] = map[string]interface{}{"text": f}
	}
	return map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "interactive",
		"interactive":       interactive,
	}
}

// MediaPayload builds an image or video message sent by link (Meta fetches the file; no bytes
// pass through this service). It reports false for another media type or an empty link.
func MediaPayload(to, mediaType, link, caption string) (map[string]interface{}, bool) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	link = strings.TrimSpace(link)
	if (mediaType != "image" && mediaType != "video") || !strings.HasPrefix(link, "https://") {
		return nil, false
	}
	media := map[string]interface{}{"link": link}
	if c := clip(caption, maxCaption); c != "" {
		media["caption"] = c
	}
	return map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              mediaType,
		mediaType:           media,
	}, true
}

// MediaFallbackText is the text sent when a media message is refused: the caption, then the link.
func MediaFallbackText(caption, link string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return link
	}
	return caption + "\n" + link
}

// richKind says which interactive or media send the metadata asks for: "buttons", "list",
// "media", or "" for none.
func richKind(metadata map[string]interface{}) string {
	if metadata == nil {
		return ""
	}
	if t, _ := metadata["media_type"].(string); t != "" {
		if l, _ := metadata["media_link"].(string); l != "" {
			return "media"
		}
	}
	if len(ReplyButtonsFrom(metadata)) > 0 {
		return "buttons"
	}
	if len(ListRowsFrom(metadata)) > 0 {
		return "list"
	}
	return ""
}

// HasRichContent reports whether the metadata asks for a reply-button, list or media message.
func HasRichContent(metadata map[string]interface{}) bool {
	return richKind(metadata) != ""
}

// sendRich sends the interactive or media message the metadata asks for. When Meta refuses it,
// the plain-text fallback goes instead, so the reader still gets the message and can answer by
// typing. handled is false when the metadata asks for none of them.
func (p *MetaCloudProvider) sendRich(ctx context.Context, to, body string, metadata map[string]interface{}) (handled bool, err error) {
	footer, _ := metadata["interactive_footer"].(string)
	fallback, _ := metadata["fallback_text"].(string)
	if strings.TrimSpace(fallback) == "" {
		fallback = body
	}
	var payload map[string]interface{}
	switch richKind(metadata) {
	case "buttons":
		payload = ButtonsPayload(to, body, footer, ReplyButtonsFrom(metadata))
	case "list":
		button, _ := metadata["interactive_list_button"].(string)
		section, _ := metadata["interactive_list_section"].(string)
		payload = ListPayload(to, body, footer, button, section, ListRowsFrom(metadata))
	case "media":
		mediaType, _ := metadata["media_type"].(string)
		link, _ := metadata["media_link"].(string)
		var ok bool
		if payload, ok = MediaPayload(to, mediaType, link, body); !ok {
			return true, p.sendText(ctx, to, MediaFallbackText(body, link), false)
		}
		fallback = MediaFallbackText(body, link)
	default:
		return false, nil
	}
	if _, err := p.post(ctx, payload); err != nil {
		if ferr := p.sendText(ctx, to, fallback, false); ferr != nil {
			return true, fmt.Errorf("%v; text fallback: %w", err, ferr)
		}
	}
	return true, nil
}
