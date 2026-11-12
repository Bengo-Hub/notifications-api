// Package tenanttemplates syncs our WhatsApp message templates to a tenant's own WhatsApp
// Business Account (WABA). Meta approves templates per WABA, so a tenant that connects its own
// number through Embedded Signup starts with none of ours and every template send from its number
// fails until they exist there. The platform WABA sync (internal/whatsapp/templatesync, route
// /platform/whatsapp/templates/sync) is unchanged; this package reuses its syncer against the
// tenant's WABA and token.
//
// Only the templates the tenant can actually send are submitted (owner decision: "Only what the
// tenant uses"), chosen by Selection:
//
//   - shared prefixes go to every tenant: auth_ (OTP, password reset), subscription_ (invoices,
//     payment due), credits_ (credit balance and top-up), broadcast_ and occasion_ (the tenant's
//     own broadcasts and occasion greetings to its customers).
//   - services map a service or product to manifest prefixes, for example ordering to ordering_
//     and erp to finance_, hrm_ and payroll_. A tenant gets a service's prefixes when that service
//     is one of its active service tags or products in the caller's token (only when the token
//     belongs to that tenant), or when its delivery logs show it was sent that service's messages
//     (the first segment of a template id: ordering/order_placed is "ordering").
//
// Platform-only templates (codevertex_ enquiries, provider_balance_low) are in no list, so they are
// never submitted to a tenant's WABA. The default map is selection.json (embedded); setting
// WHATSAPP_TENANT_TEMPLATE_MAP_FILE to a JSON file of the same shape replaces it without a release.
package tenanttemplates

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

//go:embed selection.json
var defaultSelectionJSON []byte

// SelectionFileEnv names a JSON file that replaces the embedded selection.json.
const SelectionFileEnv = "WHATSAPP_TENANT_TEMPLATE_MAP_FILE"

// Selection maps services to manifest name prefixes, plus the prefixes every tenant gets.
type Selection struct {
	Shared   []string            `json:"shared"`
	Services map[string][]string `json:"services"`
}

// LoadSelection reads the file named by WHATSAPP_TENANT_TEMPLATE_MAP_FILE when set, else the
// embedded default.
func LoadSelection() (Selection, error) {
	if path := strings.TrimSpace(os.Getenv(SelectionFileEnv)); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return Selection{}, fmt.Errorf("read %s: %w", SelectionFileEnv, err)
		}
		return ParseSelection(b)
	}
	return ParseSelection(defaultSelectionJSON)
}

// DefaultSelection is the embedded selection.json, for when an override file cannot be read.
func DefaultSelection() Selection {
	sel, _ := ParseSelection(defaultSelectionJSON) // covered by TestSelectByService
	return sel
}

// ParseSelection parses a selection map. Service keys are matched case-insensitively.
func ParseSelection(b []byte) (Selection, error) {
	var raw Selection
	if err := json.Unmarshal(b, &raw); err != nil {
		return Selection{}, fmt.Errorf("parse tenant template map: %w", err)
	}
	out := Selection{Shared: cleanPrefixes(raw.Shared), Services: map[string][]string{}}
	for k, v := range raw.Services {
		key := normalizeService(k)
		if key == "" {
			continue
		}
		out.Services[key] = cleanPrefixes(append(out.Services[key], v...))
	}
	return out, nil
}

// Prefixes returns the manifest prefixes for services (the shared ones always included) and the
// services that matched an entry, both sorted and without duplicates.
func (s Selection) Prefixes(services []string) (prefixes, matched []string) {
	set := map[string]bool{}
	for _, p := range s.Shared {
		set[p] = true
	}
	seen := map[string]bool{}
	for _, svc := range services {
		key := normalizeService(svc)
		ps, ok := s.Services[key]
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		matched = append(matched, key)
		for _, p := range ps {
			set[p] = true
		}
	}
	for p := range set {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	sort.Strings(matched)
	return prefixes, matched
}

// Select keeps the manifest definitions a tenant using services can send.
func (s Selection) Select(defs []templatesync.TemplateDef, services []string) ([]templatesync.TemplateDef, []string) {
	prefixes, matched := s.Prefixes(services)
	if len(prefixes) == 0 {
		// FilterByPrefix treats no prefixes as "everything"; an empty selection must select nothing.
		return nil, matched
	}
	return templatesync.FilterByPrefix(defs, prefixes), matched
}

// ServiceFromTemplateID is the service part of a delivery log template id: "ordering" for
// "ordering/order_placed", "" when the id has no service segment.
func ServiceFromTemplateID(id string) string {
	i := strings.Index(id, "/")
	if i <= 0 {
		return ""
	}
	return normalizeService(id[:i])
}

func normalizeService(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func cleanPrefixes(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
