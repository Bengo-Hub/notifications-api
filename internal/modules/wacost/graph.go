package wacost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultGraphBaseURL is Meta's Graph API host.
const DefaultGraphBaseURL = "https://graph.facebook.com"

// maxPricingPages bounds how many result pages one WABA may return.
const maxPricingPages = 20

// Graph reads a WABA's numbers and its pricing analytics from Meta's Graph API.
type Graph struct {
	BaseURL string
	HTTP    *http.Client
}

// PhoneNumber is one business number on a WABA.
type PhoneNumber struct {
	ID                 string `json:"id"`
	DisplayPhoneNumber string `json:"display_phone_number"`
	VerifiedName       string `json:"verified_name"`
}

// DataPoint is one pricing_analytics bucket. With the PHONE and PRICING_CATEGORY dimensions each
// point carries the number and the category; cost is in the WABA's currency.
type DataPoint struct {
	Start           int64   `json:"start"`
	End             int64   `json:"end"`
	PhoneNumber     string  `json:"phone_number"`
	PricingCategory string  `json:"pricing_category"`
	PricingType     string  `json:"pricing_type"`
	Volume          int64   `json:"volume"`
	Cost            float64 `json:"cost"`
}

// Pricing is a WABA's currency and data points for a range.
type Pricing struct {
	Currency   string
	DataPoints []DataPoint
}

// MetaError is an error answer from the Graph API, kept so the report can show Meta's words.
type MetaError struct {
	Status  int
	Message string
	Code    int
	Subcode int
}

func (e *MetaError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != 0 {
		return fmt.Sprintf("Meta refused the request: %s (code %d)", msg, e.Code)
	}
	return fmt.Sprintf("Meta refused the request: %s", msg)
}

func (g *Graph) base() string {
	if g.BaseURL != "" {
		return strings.TrimRight(g.BaseURL, "/")
	}
	return DefaultGraphBaseURL
}

func (g *Graph) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// get calls rawURL with the token as a bearer header (never in the URL) and decodes the answer.
func (g *Graph) get(ctx context.Context, rawURL, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("Meta did not answer: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read Meta answer: %w", err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
				Subcode int    `json:"error_subcode"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return &MetaError{Status: resp.StatusCode, Message: e.Error.Message, Code: e.Error.Code, Subcode: e.Error.Subcode}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode Meta answer: %w", err)
	}
	return nil
}

// PhoneNumbers lists the numbers on a WABA.
func (g *Graph) PhoneNumbers(ctx context.Context, version, wabaID, token string) ([]PhoneNumber, error) {
	q := url.Values{}
	q.Set("fields", "id,display_phone_number,verified_name")
	q.Set("limit", "100")
	var out struct {
		Data []PhoneNumber `json:"data"`
	}
	u := fmt.Sprintf("%s/%s/%s/phone_numbers?%s", g.base(), version, url.PathEscape(wabaID), q.Encode())
	if err := g.get(ctx, u, token, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// PricingAnalytics reads the WABA's currency and its daily pricing analytics for [start, end),
// broken down by phone number and pricing category:
//
//	GET /{version}/{waba}?fields=currency,pricing_analytics.start(S).end(E).granularity(DAILY)
//	    .dimensions(PHONE,PRICING_CATEGORY)
//
// See https://developers.facebook.com/docs/whatsapp/business-management-api/analytics
func (g *Graph) PricingAnalytics(ctx context.Context, version, wabaID, token string, start, end time.Time) (Pricing, error) {
	field := fmt.Sprintf("currency,pricing_analytics.start(%d).end(%d).granularity(DAILY).dimensions(PHONE,PRICING_CATEGORY)",
		start.Unix(), end.Unix())
	q := url.Values{}
	q.Set("fields", field)
	u := fmt.Sprintf("%s/%s/%s?%s", g.base(), version, url.PathEscape(wabaID), q.Encode())

	type page struct {
		Data []struct {
			DataPoints []DataPoint `json:"data_points"`
		} `json:"data"`
		Paging struct {
			Next string `json:"next"`
		} `json:"paging"`
	}
	var first struct {
		Currency         string `json:"currency"`
		PricingAnalytics page   `json:"pricing_analytics"`
	}
	if err := g.get(ctx, u, token, &first); err != nil {
		return Pricing{}, err
	}
	out := Pricing{Currency: strings.ToUpper(strings.TrimSpace(first.Currency))}
	collect := func(p page) {
		for _, d := range p.Data {
			out.DataPoints = append(out.DataPoints, d.DataPoints...)
		}
	}
	collect(first.PricingAnalytics)
	next := first.PricingAnalytics.Paging.Next
	for i := 0; next != "" && i < maxPricingPages; i++ {
		// Only follow a next link back to the same Graph host: the token goes with it.
		if !strings.HasPrefix(next, g.base()+"/") {
			return out, errors.New("Meta returned a next page on another host; the rest was not read")
		}
		var p page
		if err := g.get(ctx, next, token, &p); err != nil {
			return out, err
		}
		collect(p)
		next = p.Paging.Next
	}
	return out, nil
}

// digits keeps only the digits of a phone number, so "+254 712 345 678" and "254712345678" match.
func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
