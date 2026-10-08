// Package templatesync idempotently syncs the fleet's drafted WhatsApp message templates
// (templates.json — see the WhatsApp Template Registry review artifact) to a WhatsApp Business
// Account via Meta's Graph API. Shared by cmd/whatsapp-template-sync (CLI) and the platform admin
// HTTP endpoint (handlers.WhatsAppTemplates) so the logic — and its idempotency guarantee — exists
// in exactly one place.
package templatesync

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const apiVersion = "v25.0"

//go:embed templates.json
var manifestJSON []byte

// TemplateDef is one entry in templates.json.
type TemplateDef struct {
	Name     string   `json:"name"`
	Category string   `json:"category"` // AUTHENTICATION | UTILITY
	Language string   `json:"language"`
	Source   string   `json:"source"` // documentation only, not sent to Meta
	Body     string   `json:"body"`   // BODY component text with {{1}}, {{2}}, ... — omitted for AUTHENTICATION
	Example  []string `json:"example"`
	// Buttons, when present, adds a BUTTONS component (currently only URL buttons are used
	// anywhere in this manifest). A URL button's `url` may carry at most one dynamic
	// placeholder — always "{{1}}" — appended as a suffix to a fixed, hardcoded prefix; Meta
	// resolves the base domain once at template-approval time and can never swap it out per
	// send, so a URL button only makes sense for a domain that's the same for every send using
	// this template (see order_consumer.go's shared-vs-custom-domain template selection).
	Buttons []ButtonDef `json:"buttons,omitempty"`
}

// ButtonDef is one BUTTONS-component button. Only Type "URL" is used today.
type ButtonDef struct {
	Type    string   `json:"type"`
	Text    string   `json:"text"`
	URL     string   `json:"url,omitempty"`
	Example []string `json:"example,omitempty"`
}

// LoadManifest parses the embedded templates.json.
func LoadManifest() ([]TemplateDef, error) {
	var defs []TemplateDef
	if err := json.Unmarshal(manifestJSON, &defs); err != nil {
		return nil, fmt.Errorf("parse embedded templates.json: %w", err)
	}
	return defs, nil
}

// FilterByPrefix keeps only definitions whose name starts with one of the given prefixes. A nil or
// empty prefixes slice returns defs unchanged.
func FilterByPrefix(defs []TemplateDef, prefixes []string) []TemplateDef {
	if len(prefixes) == 0 {
		return defs
	}
	out := make([]TemplateDef, 0, len(defs))
	for _, def := range defs {
		for _, p := range prefixes {
			if strings.HasPrefix(def.Name, strings.TrimSpace(p)) {
				out = append(out, def)
				break
			}
		}
	}
	return out
}

// FilterByName keeps only definitions whose name is exactly one of names.
func FilterByName(defs []TemplateDef, names []string) []TemplateDef {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[strings.TrimSpace(n)] = true
	}
	out := make([]TemplateDef, 0, len(names))
	for _, def := range defs {
		if want[def.Name] {
			out = append(out, def)
		}
	}
	return out
}

// Outcome is one of "created", "skipped" (already existed on Meta — idempotent no-op), "deleted",
// or "failed".
type Outcome string

const (
	OutcomeCreated Outcome = "created"
	OutcomeSkipped Outcome = "skipped"
	OutcomeDeleted Outcome = "deleted"
	OutcomeFailed  Outcome = "failed"
	// OutcomeQueued is a template still to submit, left for the next batch (RunBatch).
	OutcomeQueued Outcome = "queued"
	// outcomeWouldCreate/outcomeWouldDelete are dry-run only, reported to the caller as
	// OutcomeCreated/OutcomeDeleted with DryRun=true on the Result so JSON consumers don't need
	// extra enum values to handle.
)

// Result reports what happened for one template definition.
type Result struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Outcome  Outcome `json:"outcome"`
	Detail   string  `json:"detail,omitempty"` // Meta's error message, when Outcome is "failed"
	DryRun   bool    `json:"dry_run,omitempty"`
	// MetaStatus is Meta's review status for a template that already exists (APPROVED, PENDING,
	// REJECTED, ...), so admins can see whether a submitted template can be sent yet.
	MetaStatus string `json:"meta_status,omitempty"`
	// MetaReason is Meta's rejected_reason for a REJECTED template (e.g. INVALID_FORMAT,
	// TAG_CONTENT_MISMATCH), so admins know what to change before submitting a new version.
	MetaReason string `json:"meta_reason,omitempty"`
}

// metaTemplate is one template as Meta reports it.
type metaTemplate struct {
	Status         string
	RejectedReason string
}

// Syncer talks to one WhatsApp Business Account's Graph API template endpoints.
type Syncer struct {
	WABAID string
	Token  string
	client *http.Client
	// GraphURL overrides Meta's Graph API base (tests only).
	GraphURL string
}

func (s *Syncer) graph() string {
	if s.GraphURL != "" {
		return s.GraphURL
	}
	return "https://graph.facebook.com"
}

// NewSyncer builds a Syncer. Both wabaID and token are required — there is deliberately no
// platform-level default: template management always targets a specific, explicit WABA.
func NewSyncer(wabaID, token string) *Syncer {
	return &Syncer{WABAID: wabaID, Token: token, client: &http.Client{Timeout: 15 * time.Second}}
}

// Run syncs defs against the WABA: fetches every template already registered (by name, regardless
// of review status — pending/approved/rejected all count as "already submitted", never resubmitted)
// and skips any match; creates everything else, or — when dryRun is true — reports what WOULD be
// created without calling Meta's create endpoint at all.
func (s *Syncer) Run(ctx context.Context, defs []TemplateDef, dryRun bool) ([]Result, error) {
	results, _, err := s.RunBatch(ctx, defs, dryRun, 0)
	return results, err
}

// submitGap paces creates so a batch stays well inside Meta's template-creation rate limit.
const submitGap = 400 * time.Millisecond

// RunBatch is Run with at most limit creates per call (0 = no limit). Templates past the limit come
// back as OutcomeQueued and remaining counts them, so a caller (the UI) submits the whole set in
// short requests, one after another, instead of one long request that outlives its timeout.
func (s *Syncer) RunBatch(ctx context.Context, defs []TemplateDef, dryRun bool, limit int) ([]Result, int, error) {
	existing, err := s.fetchExisting(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch existing templates from Meta: %w", err)
	}

	results := make([]Result, 0, len(defs))
	submitted, remaining := 0, 0
	for _, def := range defs {
		if mt, ok := existing[def.Name]; ok {
			reason := ""
			if mt.RejectedReason != "" && mt.RejectedReason != "NONE" {
				reason = mt.RejectedReason
			}
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeSkipped, Detail: "already exists on Meta", MetaStatus: mt.Status, MetaReason: reason})
			continue
		}
		if dryRun {
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeCreated, DryRun: true})
			continue
		}
		if limit > 0 && submitted >= limit {
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeQueued, Detail: "waiting for the next batch"})
			remaining++
			continue
		}
		if submitted > 0 {
			select {
			case <-ctx.Done():
				return results, remaining, ctx.Err()
			case <-time.After(submitGap):
			}
		}
		submitted++
		err := s.create(ctx, def)
		switch {
		case err == nil:
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeCreated, MetaStatus: "PENDING"})
		case isAlreadyExists(err):
			// An earlier attempt reached Meta even though its reply was lost: it is there.
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeSkipped, Detail: "already exists on Meta"})
		default:
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeFailed, Detail: err.Error()})
		}
	}
	return results, remaining, nil
}

// metaError is Meta's error reply, kept readable for admins rather than a raw JSON dump.
type metaError struct {
	Status  int
	Message string
	Code    int
	Subcode int
}

func (e *metaError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Meta answered %d", e.Status)
	}
	if e.Subcode != 0 {
		return fmt.Sprintf("%s (Meta error %d)", e.Message, e.Subcode)
	}
	return fmt.Sprintf("%s (Meta error %d)", e.Message, e.Code)
}

// parseMetaError reads Meta's {"error":{...}} body, preferring the message meant for users.
func parseMetaError(status int, body []byte) error {
	var r struct {
		Error struct {
			Message      string `json:"message"`
			UserTitle    string `json:"error_user_title"`
			UserMsg      string `json:"error_user_msg"`
			Code         int    `json:"code"`
			ErrorSubcode int    `json:"error_subcode"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &r) != nil {
		return &metaError{Status: status, Message: strings.TrimSpace(string(body))}
	}
	msg := r.Error.UserMsg
	if msg == "" {
		msg = r.Error.Message
	}
	if r.Error.UserTitle != "" && r.Error.UserTitle != msg {
		msg = r.Error.UserTitle + ": " + msg
	}
	return &metaError{Status: status, Message: msg, Code: r.Error.Code, Subcode: r.Error.ErrorSubcode}
}

// isAlreadyExists is Meta refusing a create because a template of that name and language exists.
func isAlreadyExists(err error) bool {
	me, ok := err.(*metaError)
	if !ok {
		return false
	}
	m := strings.ToLower(me.Message)
	return me.Subcode == 2388023 || me.Subcode == 2388024 || strings.Contains(m, "already exists") || strings.Contains(m, "being deleted")
}

// DeleteByPrefix permanently removes every template on the WABA (regardless of review status —
// approved, pending, or rejected) whose name starts with one of prefixes, fetched live from Meta
// rather than from the local manifest — so it also cleans up a template a prior manifest revision
// created and has since been renamed or removed locally. A template scheduled for deletion that's
// still actively being sent (this platform's own fallback path included) will fail sends the
// instant it's gone — callers must know that whatever they pass here has no live traffic
// depending on it, or accept the resulting gap. dryRun reports what WOULD be deleted with no
// calls to Meta's delete endpoint at all.
func (s *Syncer) DeleteByPrefix(ctx context.Context, prefixes []string, dryRun bool) ([]Result, error) {
	existing, err := s.fetchExisting(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch existing templates from Meta: %w", err)
	}
	names := make([]string, 0, len(existing))
	for name := range existing {
		for _, p := range prefixes {
			if strings.HasPrefix(name, strings.TrimSpace(p)) {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)

	results := make([]Result, 0, len(names))
	for _, name := range names {
		if dryRun {
			results = append(results, Result{Name: name, Outcome: OutcomeDeleted, DryRun: true})
			continue
		}
		if err := s.delete(ctx, name); err != nil {
			results = append(results, Result{Name: name, Outcome: OutcomeFailed, Detail: err.Error()})
			continue
		}
		results = append(results, Result{Name: name, Outcome: OutcomeDeleted})
	}
	return results, nil
}

// delete removes every template revision sharing name (Meta's delete-by-name endpoint drops all
// language variants of that name at once — this manifest only ever registers one language per
// name, so that's a non-issue here, just the documented behavior).
func (s *Syncer) delete(ctx context.Context, name string) error {
	url := fmt.Sprintf("%s/%s/%s/message_templates?name=%s", s.graph(), apiVersion, s.WABAID, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return parseMetaError(resp.StatusCode, body)
	}
	return nil
}

// fetchExisting paginates through every template already on the WABA, keyed by name.
func (s *Syncer) fetchExisting(ctx context.Context) (map[string]metaTemplate, error) {
	out := map[string]metaTemplate{}
	url := fmt.Sprintf("%s/%s/%s/message_templates?fields=name,status,rejected_reason&limit=200", s.graph(), apiVersion, s.WABAID)

	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+s.Token)

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
		}

		var lr struct {
			Data []struct {
				Name           string `json:"name"`
				Status         string `json:"status"`
				RejectedReason string `json:"rejected_reason"`
			} `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := json.Unmarshal(body, &lr); err != nil {
			return nil, fmt.Errorf("unexpected response: %s", string(body))
		}
		for _, t := range lr.Data {
			out[t.Name] = metaTemplate{Status: t.Status, RejectedReason: t.RejectedReason}
		}
		url = lr.Paging.Next
	}
	return out, nil
}

// create submits one template definition to Meta. AUTHENTICATION templates use Meta's fixed
// OTP-delivery format (no custom body text — Meta generates it); every other category sends a
// single BODY component with the drafted text and example values.
func (s *Syncer) create(ctx context.Context, def TemplateDef) error {
	payload := map[string]any{
		"name":     def.Name,
		"language": def.Language,
		"category": def.Category,
	}

	if def.Category == "AUTHENTICATION" {
		payload["components"] = []map[string]any{
			{"type": "BODY", "add_security_recommendation": true},
			{"type": "FOOTER", "code_expiration_minutes": 10},
			{"type": "BUTTONS", "buttons": []map[string]any{{"type": "OTP", "otp_type": "COPY_CODE"}}},
		}
	} else {
		components := []map[string]any{
			{
				"type": "BODY",
				"text": def.Body,
				"example": map[string]any{
					"body_text": [][]string{def.Example},
				},
			},
		}
		if len(def.Buttons) > 0 {
			buttons := make([]map[string]any, 0, len(def.Buttons))
			for _, b := range def.Buttons {
				btn := map[string]any{"type": b.Type, "text": b.Text}
				if b.URL != "" {
					btn["url"] = b.URL
				}
				if len(b.Example) > 0 {
					btn["example"] = b.Example
				}
				buttons = append(buttons, btn)
			}
			components = append(components, map[string]any{"type": "BUTTONS", "buttons": buttons})
		}
		payload["components"] = components
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/%s/%s/message_templates", s.graph(), apiVersion, s.WABAID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.Token)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return parseMetaError(resp.StatusCode, body)
	}
	return nil
}
