package broadcasts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Audience types.
const (
	AudiencePlatformTenants = "platform_tenants" // the platform's tenants (platform broadcasts only)
	AudienceTenantCustomers = "tenant_customers" // a tenant's customers, from MarketFlow
	AudienceTenantUsers     = "tenant_users"     // a tenant's own staff
)

// Address is one way to reach a person, in the order to try.
type Address struct {
	Value     string `json:"address"`
	Source    string `json:"source"` // owner, admin, tenant, main_outlet, contact
	Verified  bool   `json:"verified"`
	FirstName string `json:"first_name,omitempty"` // the person behind this address, for "Dear ..."
}

// Person is one member of an audience: a tenant (for platform broadcasts) or a customer. Emails
// and phones are ordered: the first valid one is used, the rest are backups.
type Person struct {
	Key               string // stable id, also the paging cursor
	RecipientTenantID *uuid.UUID
	BusinessName      string
	Name              string
	Region            string // ISO country for reading local phone numbers
	Emails            []Address
	Phones            []Address
	// Marketing consent, when the audience source tracks it (customers). nil = not tracked.
	// false means the person opted out: never message them, whatever the sender attests.
	EmailConsent *bool
	SMSConsent   *bool
	// ConsentRecorded says when and how consent was given is on file. Contacts from before
	// consent was recorded only get marketing when the sender attests they agreed.
	ConsentRecorded bool
}

// marketingBlocked says why a person may not get marketing on a channel ("" when they may).
func marketingBlocked(consent *bool, recorded, attested bool) string {
	if consent == nil {
		return "" // the source does not track consent (tenants, staff)
	}
	if !*consent {
		return "opted out of marketing"
	}
	if !recorded && !attested {
		return "no recorded marketing consent"
	}
	return ""
}

// Resolver pages through an audience. after is the last Key of the previous page ("" for the
// first); next is "" when there are no more pages.
type Resolver interface {
	Page(ctx context.Context, audience map[string]any, senderTenant *uuid.UUID, after string, limit int) (people []Person, next string, err error)
}

// ErrAudienceUnavailable is returned for an audience this deployment cannot resolve (its source
// service does not offer the list). Retrying does not help, so a broadcast stops with it.
var ErrAudienceUnavailable = errors.New("this audience's contact list is not available")

// AuthReach resolves platform_tenants through auth-api's
// GET /api/v1/s2s/tenants/reach?purpose=broadcast (owners and admins with verified contacts
// first, then the tenant's configured contacts, then the main outlet's).
type AuthReach struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

type reachResponse struct {
	Data []struct {
		Key          string    `json:"key"` // cursor: tenant id (broadcast) or user id (staff)
		TenantID     string    `json:"tenant_id"`
		Slug         string    `json:"slug"`
		Name         string    `json:"name"` // the person, for staff entries
		BusinessName string    `json:"business_name"`
		Country      string    `json:"country"`
		Emails       []Address `json:"emails"`
		Phones       []Address `json:"phones"`
	} `json:"data"`
	Next string `json:"next"`
}

// Page implements Resolver for AudiencePlatformTenants.
func (a *AuthReach) Page(ctx context.Context, audience map[string]any, _ *uuid.UUID, after string, limit int) ([]Person, string, error) {
	q := url.Values{}
	q.Set("purpose", "broadcast")
	filters, _ := audience["filters"].(map[string]any)
	for _, k := range []string{"plan", "use_case", "tenant_ids"} {
		if vs := stringList(filters[k]); len(vs) > 0 {
			q.Set(k, strings.Join(vs, ","))
		}
	}
	if b, ok := filters["include_demo"].(bool); ok && b {
		q.Set("include_demo", "true")
	}
	return a.page(ctx, q, after, limit)
}

// page calls GET /api/v1/s2s/tenants/reach with q plus paging.
func (a *AuthReach) page(ctx context.Context, q url.Values, after string, limit int) ([]Person, string, error) {
	if a.BaseURL == "" || a.APIKey == "" {
		return nil, "", fmt.Errorf("auth-api url or internal key not configured")
	}
	q.Set("limit", strconv.Itoa(limit))
	if after != "" {
		q.Set("after", after)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(a.BaseURL, "/")+"/api/v1/s2s/tenants/reach?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-API-Key", a.APIKey)
	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("auth tenants reach: status %d", resp.StatusCode)
	}
	var out reachResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	people := make([]Person, 0, len(out.Data))
	for _, t := range out.Data {
		key := t.Key
		if key == "" {
			key = t.TenantID
		}
		name := t.Name
		if name == "" {
			name = t.BusinessName
		}
		p := Person{Key: key, BusinessName: t.BusinessName, Name: name, Region: t.Country, Emails: t.Emails, Phones: t.Phones}
		if id, err := uuid.Parse(t.TenantID); err == nil {
			p.RecipientTenantID = &id
		}
		people = append(people, p)
	}
	return people, out.Next, nil
}

func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if l == "" {
			return nil
		}
		return strings.Split(l, ",")
	}
	return nil
}
