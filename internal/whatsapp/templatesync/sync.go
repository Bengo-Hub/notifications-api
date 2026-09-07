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

// Outcome is one of "created", "skipped" (already existed on Meta — idempotent no-op), or "failed".
type Outcome string

const (
	OutcomeCreated Outcome = "created"
	OutcomeSkipped Outcome = "skipped"
	OutcomeFailed  Outcome = "failed"
	// outcomeWouldCreate is dry-run only, reported to the caller as OutcomeCreated with DryRun=true
	// on the Result so JSON consumers don't need a fourth enum value to handle.
)

// Result reports what happened for one template definition.
type Result struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Outcome  Outcome `json:"outcome"`
	Detail   string  `json:"detail,omitempty"` // Meta's error message, when Outcome is "failed"
	DryRun   bool    `json:"dry_run,omitempty"`
}

// Syncer talks to one WhatsApp Business Account's Graph API template endpoints.
type Syncer struct {
	WABAID string
	Token  string
	client *http.Client
}

// NewSyncer builds a Syncer. Both wabaID and token are required — there is deliberately no
// platform-level default: template management always targets a specific, explicit WABA.
func NewSyncer(wabaID, token string) *Syncer {
	return &Syncer{WABAID: wabaID, Token: token, client: &http.Client{Timeout: 20 * time.Second}}
}

// Run syncs defs against the WABA: fetches every template already registered (by name, regardless
// of review status — pending/approved/rejected all count as "already submitted", never resubmitted)
// and skips any match; creates everything else, or — when dryRun is true — reports what WOULD be
// created without calling Meta's create endpoint at all.
func (s *Syncer) Run(ctx context.Context, defs []TemplateDef, dryRun bool) ([]Result, error) {
	existing, err := s.fetchExisting(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch existing templates from Meta: %w", err)
	}

	results := make([]Result, 0, len(defs))
	for _, def := range defs {
		if _, ok := existing[def.Name]; ok {
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeSkipped, Detail: "already exists on Meta"})
			continue
		}
		if dryRun {
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeCreated, DryRun: true})
			continue
		}
		if err := s.create(ctx, def); err != nil {
			results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeFailed, Detail: err.Error()})
			continue
		}
		results = append(results, Result{Name: def.Name, Category: def.Category, Outcome: OutcomeCreated})
	}
	return results, nil
}

// fetchExisting paginates through every template already on the WABA, keyed by name.
func (s *Syncer) fetchExisting(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/message_templates?fields=name,status&limit=200", apiVersion, s.WABAID)

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
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := json.Unmarshal(body, &lr); err != nil {
			return nil, fmt.Errorf("unexpected response: %s", string(body))
		}
		for _, t := range lr.Data {
			out[t.Name] = t.Status
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
		payload["components"] = []map[string]any{
			{
				"type": "BODY",
				"text": def.Body,
				"example": map[string]any{
					"body_text": [][]string{def.Example},
				},
			},
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/message_templates", apiVersion, s.WABAID)
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
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
