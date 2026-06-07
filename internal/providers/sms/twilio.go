package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const twilioBaseURL = "https://api.twilio.com/2010-04-01"

var providerHTTPClient = &http.Client{Timeout: 15 * time.Second}

// SendWithTwilio sends SMS messages using the Twilio REST API.
func SendWithTwilio(ctx context.Context, accountSID, authToken, from string, to []string, body string) error {
	if accountSID == "" || authToken == "" {
		return fmt.Errorf("twilio credentials not configured")
	}

	endpoint := fmt.Sprintf("%s/Accounts/%s/Messages.json", twilioBaseURL, accountSID)

	var lastErr error
	for _, recipient := range to {
		form := url.Values{}
		form.Set("To", recipient)
		form.Set("From", from)
		form.Set("Body", body)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			lastErr = fmt.Errorf("twilio: create request: %w", err)
			continue
		}
		req.SetBasicAuth(accountSID, authToken)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := providerHTTPClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("twilio: send request for %s: %w", recipient, err)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 300 {
			var errResp struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			}
			_ = json.Unmarshal(respBody, &errResp)
			lastErr = fmt.Errorf("twilio: status %d for %s: %s (code %d)", resp.StatusCode, recipient, errResp.Message, errResp.Code)
		}
	}
	return lastErr
}
