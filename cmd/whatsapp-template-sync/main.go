// Command whatsapp-template-sync idempotently syncs the fleet's WhatsApp message templates
// (templates.json, drafted from the notifications-api template store — see the WhatsApp Template
// Registry review artifact) to Meta's WhatsApp Business Account via the Graph API.
//
// Idempotent by design: before creating anything, it fetches every template already registered on
// the WABA and skips any name that already exists there — running this twice, or after a partial
// failure, never submits a duplicate. It only ever creates; it never edits or deletes an existing
// template (editing an already-approved template resets its review status on Meta's side, which is
// a real, external-facing action this tool deliberately doesn't take without a human choosing to).
//
// Usage:
//
//	WHATSAPP_WABA_ID=... WHATSAPP_ACCESS_TOKEN=... go run ./cmd/whatsapp-template-sync -dry-run
//	WHATSAPP_WABA_ID=... WHATSAPP_ACCESS_TOKEN=... go run ./cmd/whatsapp-template-sync
//
// -dry-run prints exactly what would be created (and what's already present, skipped) without
// calling Meta's create endpoint at all — always run this first.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const apiVersion = "v25.0"

//go:embed templates.json
var manifestJSON []byte

// templateDef is one entry in templates.json.
type templateDef struct {
	Name     string   `json:"name"`
	Category string   `json:"category"` // AUTHENTICATION | UTILITY
	Language string   `json:"language"`
	Source   string   `json:"source"` // documentation only, not sent to Meta
	Body     string   `json:"body"`   // BODY component text with {{1}}, {{2}}, ... — omitted for AUTHENTICATION
	Example  []string `json:"example"`
}

// existingTemplate is the subset of fields read back from Meta to check what's already registered.
type existingTemplate struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type listResponse struct {
	Data   []existingTemplate `json:"data"`
	Paging struct {
		Next string `json:"next"`
	} `json:"paging"`
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print what would be created without calling Meta")
	flag.Parse()

	wabaID := os.Getenv("WHATSAPP_WABA_ID")
	token := os.Getenv("WHATSAPP_ACCESS_TOKEN")
	if wabaID == "" || token == "" {
		fmt.Fprintln(os.Stderr, "WHATSAPP_WABA_ID and WHATSAPP_ACCESS_TOKEN must both be set")
		os.Exit(1)
	}

	var defs []templateDef
	if err := json.Unmarshal(manifestJSON, &defs); err != nil {
		fmt.Fprintf(os.Stderr, "parse embedded templates.json: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("loaded %d template definitions\n\n", len(defs))

	client := &http.Client{Timeout: 20 * time.Second}

	existing, err := fetchExistingTemplates(client, wabaID, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch existing templates from Meta: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%d templates already registered on WABA %s:\n", len(existing), wabaID)
	for name, status := range existing {
		fmt.Printf("  - %s (%s)\n", name, status)
	}
	fmt.Println()

	created, skipped, failed := 0, 0, 0
	for _, def := range defs {
		if _, ok := existing[def.Name]; ok {
			fmt.Printf("SKIP  %-38s already exists on Meta (idempotent)\n", def.Name)
			skipped++
			continue
		}

		if *dryRun {
			fmt.Printf("WOULD CREATE  %-30s category=%s language=%s\n", def.Name, def.Category, def.Language)
			continue
		}

		if err := createTemplate(client, wabaID, token, def); err != nil {
			fmt.Printf("FAIL  %-38s %v\n", def.Name, err)
			failed++
			continue
		}
		fmt.Printf("CREATED  %-35s submitted for Meta review\n", def.Name)
		created++
	}

	fmt.Println()
	if *dryRun {
		fmt.Println("dry run — nothing was sent to Meta")
		return
	}
	fmt.Printf("done: %d created, %d skipped (already existed), %d failed\n", created, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// fetchExistingTemplates paginates through every template already on the WABA, keyed by name, so
// the caller can skip anything already present regardless of its current review status (pending,
// approved, or rejected all count as "already submitted" — this tool never resubmits).
func fetchExistingTemplates(client *http.Client, wabaID, token string) (map[string]string, error) {
	out := map[string]string{}
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/message_templates?fields=name,status&limit=200", apiVersion, wabaID)

	for url != "" {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
		}

		var lr listResponse
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

// createTemplate submits one template definition to Meta. AUTHENTICATION templates use Meta's
// fixed OTP-delivery format (no custom body text — Meta generates it); every other category sends
// a single BODY component with the drafted text and example values.
func createTemplate(client *http.Client, wabaID, token string, def templateDef) error {
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

	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/message_templates", apiVersion, wabaID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
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
