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

// ResolverConfig is where audiences come from.
type ResolverConfig struct {
	AuthAPI       string
	MarketflowAPI string
	// MaskaniAPI serves estate residents for maskani_residents audiences.
	MaskaniAPI string
	APIKey        string // INTERNAL_SERVICE_KEY, sent as X-API-Key
	// PlatformTenantID is the platform's own tenant (codevertex), never a recipient of a
	// platform broadcast.
	PlatformTenantID string
	// ExcludedTenants never receive platform broadcasts (see AuthReach.ExcludedTenants).
	ExcludedTenants []string
}

// DefaultResolvers is the one place audience types are wired, used by both the API (estimates)
// and the worker (sending), so the two never disagree about who an audience is.
func DefaultResolvers(c ResolverConfig) map[string]Resolver {
	client := &http.Client{Timeout: 25 * time.Second}
	auth := &AuthReach{BaseURL: c.AuthAPI, APIKey: c.APIKey, HTTP: client, PlatformTenantID: c.PlatformTenantID, ExcludedTenants: c.ExcludedTenants}
	return map[string]Resolver{
		AudiencePlatformTenants: auth,
		AudienceTenantUsers:     &AuthStaff{Reach: auth},
		AudienceTenantCustomers: &MarketflowContacts{BaseURL: c.MarketflowAPI, APIKey: c.APIKey, HTTP: client},
		AudienceMaskaniResidents: &MaskaniResidents{BaseURL: c.MaskaniAPI, APIKey: c.APIKey, HTTP: client},
	}
}

// MaskaniResidents resolves maskani_residents from maskani-api, which owns estate parties and
// their contact details: GET /api/v1/internal/residents/reach. Estate notices are service
// messages, so no marketing consent applies (EmailConsent and SMSConsent stay nil).
type MaskaniResidents struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

type maskaniReachResponse struct {
	Data []struct {
		Key       string `json:"key"`
		Name      string `json:"name"`
		FirstName string `json:"first_name"`
		Email     string `json:"email"`
		Phone     string `json:"phone"`
	} `json:"data"`
	Next string `json:"next"`
}

// Page implements Resolver for AudienceMaskaniResidents.
func (m *MaskaniResidents) Page(ctx context.Context, audience map[string]any, senderTenant *uuid.UUID, after string, limit int) ([]Person, string, error) {
	if senderTenant == nil {
		return nil, "", errors.New("estate notices need a sending tenant")
	}
	if m.BaseURL == "" || m.APIKey == "" {
		return nil, "", ErrAudienceUnavailable
	}
	q := url.Values{}
	q.Set("tenant_id", senderTenant.String())
	q.Set("limit", strconv.Itoa(limit))
	if after != "" {
		q.Set("after", after)
	}
	if pid, _ := audience["property_id"].(string); pid != "" {
		q.Set("property_id", pid)
	}
	for _, k := range []string{"block_ids", "unit_ids", "roles"} {
		if vs := stringList(audience[k]); len(vs) > 0 {
			q.Set(k, strings.Join(vs, ","))
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(m.BaseURL, "/")+"/api/v1/internal/residents/reach?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-API-Key", m.APIKey)
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, "", fmt.Errorf("%w: maskani-api does not offer the resident list", ErrAudienceUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("maskani residents reach: status %d", resp.StatusCode)
	}
	var out maskaniReachResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	people := make([]Person, 0, len(out.Data))
	for _, r := range out.Data {
		p := Person{Key: r.Key, Name: r.Name, Region: "KE"}
		if r.Email != "" {
			p.Emails = []Address{{Value: r.Email, Source: "contact", FirstName: r.FirstName}}
		}
		if r.Phone != "" {
			p.Phones = []Address{{Value: r.Phone, Source: "contact", FirstName: r.FirstName}}
		}
		people = append(people, p)
	}
	return people, out.Next, nil
}

// AuthStaff resolves tenant_users: the sending tenant's active members with verified contacts,
// from the same auth-api endpoint (purpose=staff).
type AuthStaff struct {
	Reach *AuthReach
}

// Page implements Resolver for AudienceTenantUsers.
func (a *AuthStaff) Page(ctx context.Context, audience map[string]any, senderTenant *uuid.UUID, after string, limit int) ([]Person, string, error) {
	if senderTenant == nil {
		return nil, "", errors.New("staff messages need a sending tenant")
	}
	q := url.Values{}
	q.Set("purpose", "staff")
	q.Set("tenant_ids", senderTenant.String())
	if roles := stringList(audience["roles"]); len(roles) > 0 {
		q.Set("roles", strings.Join(roles, ","))
	}
	return a.Reach.page(ctx, q, after, limit)
}

// MarketflowContacts resolves tenant_customers from MarketFlow, which owns customer contacts and
// their marketing consent: GET /api/v1/internal/contacts/reach.
type MarketflowContacts struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

type mfReachResponse struct {
	Data []struct {
		ID              string `json:"id"`
		FirstName       string `json:"first_name"`
		LastName        string `json:"last_name"`
		Email           string `json:"email"`
		Phone           string `json:"phone"`
		Country         string `json:"country"`
		EmailConsent    bool   `json:"subscribed_email"`
		SMSConsent      bool   `json:"subscribed_sms"`
		ConsentRecorded bool   `json:"consent_recorded"`
		EmailConfirmed  bool   `json:"email_verified"`
	} `json:"data"`
	Next string `json:"next"`
}

// Page implements Resolver for AudienceTenantCustomers.
func (m *MarketflowContacts) Page(ctx context.Context, audience map[string]any, senderTenant *uuid.UUID, after string, limit int) ([]Person, string, error) {
	if senderTenant == nil {
		return nil, "", errors.New("customer messages need a sending tenant")
	}
	if m.BaseURL == "" || m.APIKey == "" {
		return nil, "", ErrAudienceUnavailable
	}
	q := url.Values{}
	q.Set("tenant_id", senderTenant.String())
	q.Set("limit", strconv.Itoa(limit))
	if after != "" {
		q.Set("after", after)
	}
	if seg, _ := audience["segment_id"].(string); seg != "" {
		q.Set("segment_id", seg)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(m.BaseURL, "/")+"/api/v1/internal/contacts/reach?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-API-Key", m.APIKey)
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	// 404/405: this MarketFlow does not serve the reach endpoint (or the route is gone).
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, "", fmt.Errorf("%w: MarketFlow does not offer the customer list yet", ErrAudienceUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("marketflow contacts reach: status %d", resp.StatusCode)
	}
	var out mfReachResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	people := make([]Person, 0, len(out.Data))
	for _, c := range out.Data {
		name := strings.TrimSpace(c.FirstName + " " + c.LastName)
		emailOK, smsOK := c.EmailConsent, c.SMSConsent
		p := Person{Key: c.ID, Name: name, Region: c.Country, EmailConsent: &emailOK, SMSConsent: &smsOK, ConsentRecorded: c.ConsentRecorded}
		if c.Email != "" {
			p.Emails = []Address{{Value: c.Email, Source: "contact", Verified: c.EmailConfirmed, FirstName: c.FirstName}}
		}
		if c.Phone != "" {
			p.Phones = []Address{{Value: c.Phone, Source: "contact", FirstName: c.FirstName}}
		}
		people = append(people, p)
	}
	return people, out.Next, nil
}
