package maildelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProvisionerApplier returns an Applier that asks email-provisioner, the only service holding the
// mail server's admin login, to create or update Stalwart's delivery webhook with the given key.
// baseURL is the provisioner's cluster address and webhookURL is where Stalwart should post.
func ProvisionerApplier(baseURL, webhookURL string, client *http.Client) Applier {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/internal/delivery-webhook"
	return func(ctx context.Context, key string) error {
		body, err := json.Marshal(map[string]string{"url": webhookURL, "key": key})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("email-provisioner unreachable: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			return fmt.Errorf("email-provisioner answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		return nil
	}
}
