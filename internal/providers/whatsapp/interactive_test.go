package whatsapp

import (
	"encoding/json"
	"strings"
	"testing"
)

// roundTrip passes metadata through JSON, as it arrives after the NATS event bus.
func roundTrip(t *testing.T, m map[string]interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReplyButtonsFromKeepsThreeValidOnes(t *testing.T) {
	meta := roundTrip(t, map[string]interface{}{"interactive_buttons": []map[string]string{
		{"id": "mf:yes:run1:1234", "title": "Approve"},
		{"id": "", "title": "No id"},
		{"id": "mf:cheaper:run1", "title": "Cheaper option, please, right now"},
		{"id": "mf:cancel:run1", "title": "Cancel"},
		{"id": "mf:extra", "title": "Fourth"},
	}})
	got := ReplyButtonsFrom(meta)
	if len(got) != 3 {
		t.Fatalf("want 3 buttons, got %d: %+v", len(got), got)
	}
	if got[0].ID != "mf:yes:run1:1234" || got[2].ID != "mf:cancel:run1" {
		t.Fatalf("order or ids wrong: %+v", got)
	}
	if n := len([]rune(got[1].Title)); n > maxButtonTitle {
		t.Fatalf("title not clipped to %d: %q", maxButtonTitle, got[1].Title)
	}
}

func TestButtonsPayloadShape(t *testing.T) {
	p := ButtonsPayload("254712345678", strings.Repeat("a", 1500), "Codes work for 30 minutes",
		[]ReplyButton{{ID: "mf:yes:r:1234", Title: "Approve"}, {ID: "mf:cancel:r", Title: "Cancel"}})
	if p["type"] != "interactive" || p["to"] != "254712345678" {
		t.Fatalf("envelope: %v", p)
	}
	in := p["interactive"].(map[string]interface{})
	if in["type"] != "button" {
		t.Fatalf("type: %v", in["type"])
	}
	if body := in["body"].(map[string]interface{})["text"].(string); len([]rune(body)) != maxInteractiveBody {
		t.Fatalf("body not clipped: %d", len([]rune(body)))
	}
	btns := in["action"].(map[string]interface{})["buttons"].([]map[string]interface{})
	if len(btns) != 2 || btns[0]["type"] != "reply" || btns[0]["reply"].(map[string]interface{})["id"] != "mf:yes:r:1234" {
		t.Fatalf("buttons: %v", btns)
	}
	if in["footer"] == nil {
		t.Fatal("footer missing")
	}
}

func TestListPayloadShape(t *testing.T) {
	meta := roundTrip(t, map[string]interface{}{"interactive_list_rows": []map[string]string{
		{"id": "mf:f:objective:0", "title": "Sales", "description": "People buy from the ad"},
		{"id": "mf:f:objective:1", "title": "Leads"},
	}})
	rows := ListRowsFrom(meta)
	p := ListPayload("254712345678", "What result do you want?", "", "", "Objective", rows)
	in := p["interactive"].(map[string]interface{})
	if in["type"] != "list" {
		t.Fatalf("type: %v", in["type"])
	}
	action := in["action"].(map[string]interface{})
	if action["button"] != defaultListButtonTxt {
		t.Fatalf("default list button: %v", action["button"])
	}
	secs := action["sections"].([]map[string]interface{})
	items := secs[0]["rows"].([]map[string]interface{})
	if len(items) != 2 || items[0]["id"] != "mf:f:objective:0" || items[0]["description"] == nil || items[1]["description"] != nil {
		t.Fatalf("rows: %v", items)
	}
	if _, ok := in["footer"]; ok {
		t.Fatal("an empty footer must be left out")
	}
}

func TestListRowsFromCapsAtTen(t *testing.T) {
	var rows []interface{}
	for i := 0; i < 14; i++ {
		rows = append(rows, map[string]interface{}{"id": "r", "title": "Row"})
	}
	if got := ListRowsFrom(map[string]interface{}{"interactive_list_rows": rows}); len(got) != maxListRows {
		t.Fatalf("want %d rows, got %d", maxListRows, len(got))
	}
}

func TestMediaPayload(t *testing.T) {
	p, ok := MediaPayload("254712345678", "Video", "https://cdn.example.com/ad.mp4", "Your video ad is ready")
	if !ok || p["type"] != "video" {
		t.Fatalf("video payload: %v %v", ok, p)
	}
	v := p["video"].(map[string]interface{})
	if v["link"] != "https://cdn.example.com/ad.mp4" || v["caption"] != "Your video ad is ready" {
		t.Fatalf("video body: %v", v)
	}
	if _, ok := MediaPayload("254712345678", "document", "https://cdn.example.com/a.pdf", ""); ok {
		t.Fatal("only image and video are sent as media")
	}
	if _, ok := MediaPayload("254712345678", "image", "http://cdn.example.com/a.png", ""); ok {
		t.Fatal("a link Meta cannot fetch over https must not be sent")
	}
	if got := MediaFallbackText("Scene 1", "https://x/1.png"); got != "Scene 1\nhttps://x/1.png" {
		t.Fatalf("fallback: %q", got)
	}
}

func TestRichKind(t *testing.T) {
	cases := []struct {
		meta map[string]interface{}
		want string
	}{
		{nil, ""},
		{map[string]interface{}{"cta_button_text": "Open"}, ""},
		{map[string]interface{}{"media_type": "image"}, ""},
		{map[string]interface{}{"media_type": "image", "media_link": "https://x/1.png"}, "media"},
		{map[string]interface{}{"interactive_buttons": []interface{}{map[string]interface{}{"id": "a", "title": "A"}}}, "buttons"},
		{map[string]interface{}{"interactive_list_rows": []interface{}{map[string]interface{}{"id": "a", "title": "A"}}}, "list"},
	}
	for i, tc := range cases {
		if got := richKind(tc.meta); got != tc.want {
			t.Fatalf("case %d: richKind = %q, want %q", i, got, tc.want)
		}
	}
}
