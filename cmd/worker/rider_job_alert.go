package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/devicetoken"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// riderJobData is what the rider sees about a job assigned to them.
func riderJobData(payload map[string]interface{}, tenantSlug, riderAppURL string) map[string]interface{} {
	orderNo, _ := payload["order_number"].(string)
	if orderNo == "" {
		orderNo, _ = payload["tracking_code"].(string)
	}
	pickup, _ := payload["pickup_name"].(string)
	riderName, _ := payload["rider_name"].(string)
	path := "/active"
	if tenantSlug != "" {
		path = "/" + tenantSlug + "/active"
	}
	cod := ""
	if v, ok := payload["cash_on_delivery"].(float64); ok && v > 0 {
		cod = fmt.Sprintf("%.2f", v)
	}
	return map[string]interface{}{
		"name":             firstNonEmpty(riderName, "there"),
		"order_number":     orderNo,
		"pickup_name":      pickup,
		"cash_on_delivery": cod,
		// Relative path for the push click (opens inside the rider app); full link for email.
		"url":      path,
		"job_link": strings.TrimRight(riderAppURL, "/") + path,
		"type":     "rider_job_assigned",
	}
}

// notifyRiderAssigned tells the rider a delivery job is theirs: a push to every device they
// registered in the rider app, and an email when their address is known. Before this the rider
// got nothing and only found the job by opening the app.
func notifyRiderAssigned(ctx context.Context, nc *nats.Conn, cfg *config.Config, client *ent.Client, ti *tenantInfo, evt deliveryEvent, logg *zap.Logger) {
	taskID, _ := evt.Payload["task_id"].(string)
	riderAppURL := firstNonEmpty(ti.ServiceURLs["rider"], serviceURL("NOTIFICATIONS_RIDER_APP_URL", "https://riderapp.codevertexafrica.com"))
	data := riderJobData(evt.Payload, ti.Slug, riderAppURL)

	if riderUser, _ := evt.Payload["rider_user_id"].(string); riderUser != "" && client != nil {
		userID, uerr := uuid.Parse(riderUser)
		tenantID, terr := uuid.Parse(evt.TenantID)
		if uerr == nil && terr == nil {
			tokens, err := client.DeviceToken.Query().
				Where(devicetoken.TenantID(tenantID), devicetoken.UserID(userID), devicetoken.IsActive(true)).
				All(ctx)
			if err != nil {
				logg.Warn("rider job push: device token lookup failed", zap.String("task_id", taskID), zap.Error(err))
			} else if len(tokens) > 0 {
				toks := make([]string, 0, len(tokens))
				for _, t := range tokens {
					toks = append(toks, t.Token)
				}
				msg := messaging.Message{
					TenantID:       evt.TenantID,
					Channel:        "push",
					TemplateID:     "logistics/rider_job_assigned",
					SenderScope:    messaging.SenderScopeTenant,
					Target:         messaging.TargetStaff,
					To:             messaging.NormalizeRecipients(toks, "push"),
					Data:           data,
					Metadata:       map[string]any{"push_title": "New delivery job"},
					RequestID:      uuid.New().String(),
					IdempotencyKey: fmt.Sprintf("rider-job-push-%s-%s", taskID, riderUser),
					QueuedAt:       time.Now(),
				}
				if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
					logg.Warn("rider job push: publish failed", zap.String("task_id", taskID), zap.Error(err))
				}
			}
		}
	}

	if email, _ := evt.Payload["rider_email"].(string); email != "" {
		msg := messaging.Message{
			TenantID:       evt.TenantID,
			Channel:        "email",
			TemplateID:     "logistics/rider_job_assigned",
			SenderScope:    messaging.SenderScopeTenant,
			Target:         messaging.TargetStaff,
			To:             []string{email},
			Data:           data,
			Metadata:       map[string]any{"subject": "New delivery job " + fmt.Sprint(data["order_number"])},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("rider-job-email-%s-%s", taskID, email),
			QueuedAt:       time.Now(),
		}
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Warn("rider job email: publish failed", zap.String("task_id", taskID), zap.Error(err))
		}
	}
}
