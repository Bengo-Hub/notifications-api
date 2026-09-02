package events

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent/devicetoken"
	"github.com/bengobox/notifications-api/internal/ent/user"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// labOrderCriticalResultPayload is hospital-api's hospital.lab_order.critical_result
// payload (see hospital-api's internal/modules/lab/service.go EnterResult and the
// constant events.EventLabOrderCriticalResult in hospital-api's internal/events/publish.go).
type labOrderCriticalResultPayload struct {
	LabOrderID     string `json:"lab_order_id"`
	LabOrderLineID string `json:"lab_order_line_id"`
	OrderedBy      string `json:"ordered_by"`
	TestName       string `json:"test_name"`
	ResultValue    string `json:"result_value"`
	Unit           string `json:"unit"`
	ReferenceRange string `json:"reference_range"`
}

// handleHospitalLabOrderCriticalResult sends an urgent SMS + best-effort push alert to the
// ordering clinician the instant a lab result comes back flagged critical — a Joint Commission
// National Patient Safety Goal, so this must not wait for (or be conflated with) the routine
// "results ready" flow.
//
// Recipient resolution: `ordered_by` on the event is hospital-api's LabOrder.ordered_by, which
// hospital-api's handlers/lab.go populates straight from the requester's JWT `sub` claim (see
// currentUserID in hospital-api's internal/http/handlers/patients.go) — i.e. it IS the platform
// auth-service user id, NOT hospital-api's own HospitalUser.id (a locally-generated row id that,
// since a 2026-08-30 fix, is deliberately distinct from auth_service_user_id — see hospital-api's
// internal/ent/schema/hospital_user.go). That auth-service user id is exactly what this service's
// own ent.User.auth_service_user_id (globally unique — see internal/ent/schema/user.go) and
// ent.DeviceToken.user_id are keyed on, both kept in sync via the identity module's
// auth.user.created/updated NATS consumer (internal/modules/identity/events.go). So the clinician
// is resolved with a plain local lookup — no S2S call to hospital-api or auth-api needed.
func (s *Subscriber) handleHospitalLabOrderCriticalResult(msg *nats.Msg) {
	var envelope struct {
		TenantID string                        `json:"tenant_id"`
		Payload  labOrderCriticalResultPayload `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		s.log.Warn("lab_order_critical_result: unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	p := envelope.Payload

	if envelope.TenantID == "" || p.OrderedBy == "" {
		s.log.Warn("lab_order_critical_result: missing tenant_id/ordered_by, dropping",
			zap.String("lab_order_line_id", p.LabOrderLineID))
		_ = msg.Ack()
		return
	}
	tenantID, err := uuid.Parse(envelope.TenantID)
	if err != nil {
		s.log.Warn("lab_order_critical_result: invalid tenant_id, dropping",
			zap.String("tenant_id", envelope.TenantID), zap.Error(err))
		_ = msg.Ack()
		return
	}
	orderedBy, err := uuid.Parse(p.OrderedBy)
	if err != nil {
		s.log.Warn("lab_order_critical_result: invalid ordered_by, dropping",
			zap.String("ordered_by", p.OrderedBy), zap.Error(err))
		_ = msg.Ack()
		return
	}
	if s.entClient == nil {
		s.log.Warn("lab_order_critical_result: no ent client wired, cannot resolve recipient",
			zap.String("tenant_id", envelope.TenantID))
		_ = msg.Ack()
		return
	}

	data := map[string]any{
		"lab_order_id":      p.LabOrderID,
		"lab_order_line_id": p.LabOrderLineID,
		"test_name":         p.TestName,
		"result_value":      p.ResultValue,
		"unit":              p.Unit,
		"reference_range":   p.ReferenceRange,
	}

	ctx := context.Background()
	sent := false

	// SMS — the primary channel: an urgent clinical alert must reach a phone directly.
	clinician, err := s.entClient.User.Query().
		Where(user.AuthServiceUserIDEQ(orderedBy)).
		Only(ctx)
	switch {
	case err == nil && strings.TrimSpace(clinician.Phone) != "":
		s.publish(envelope.TenantID, "sms", "hospital/lab_order_critical_result", messaging.TargetStaff, []string{clinician.Phone}, data)
		sent = true
	case err == nil:
		s.log.Warn("lab_order_critical_result: ordering clinician has no phone on file",
			zap.String("tenant_id", envelope.TenantID), zap.String("ordered_by", p.OrderedBy))
	default:
		s.log.Warn("lab_order_critical_result: could not resolve ordering clinician contact",
			zap.String("tenant_id", envelope.TenantID), zap.String("ordered_by", p.OrderedBy), zap.Error(err))
	}

	// Push — best-effort in addition to SMS: reaches the clinician's app even if the
	// tenant is out of SMS credit (push has no per-tenant credit gate in the worker).
	tokens, terr := s.entClient.DeviceToken.Query().
		Where(devicetoken.TenantID(tenantID), devicetoken.UserID(orderedBy), devicetoken.IsActive(true)).
		All(ctx)
	if terr != nil {
		s.log.Warn("lab_order_critical_result: device token lookup failed",
			zap.String("tenant_id", envelope.TenantID), zap.String("ordered_by", p.OrderedBy), zap.Error(terr))
	} else if len(tokens) > 0 {
		toks := make([]string, 0, len(tokens))
		for _, t := range tokens {
			toks = append(toks, t.Token)
		}
		pushMsg := messaging.Message{
			TenantID:    envelope.TenantID,
			Channel:     "push",
			TemplateID:  "hospital/lab_order_critical_result",
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetStaff,
			To:          messaging.NormalizeRecipients(toks, "push"),
			Data:        data,
			Metadata:    map[string]any{"push_title": "Critical Lab Result"},
		}
		if pubErr := s.publishMessage(pushMsg); pubErr != nil {
			s.log.Warn("lab_order_critical_result: push publish failed",
				zap.String("tenant_id", envelope.TenantID), zap.String("ordered_by", p.OrderedBy), zap.Error(pubErr))
		} else {
			sent = true
		}
	}

	if !sent {
		s.log.Warn("lab_order_critical_result: no deliverable channel for ordering clinician (no phone, no active device token)",
			zap.String("tenant_id", envelope.TenantID),
			zap.String("ordered_by", p.OrderedBy),
			zap.String("lab_order_line_id", p.LabOrderLineID))
	} else {
		s.log.Info("lab_order_critical_result alert dispatched",
			zap.String("tenant_id", envelope.TenantID),
			zap.String("ordered_by", p.OrderedBy),
			zap.String("test_name", p.TestName),
			zap.String("lab_order_line_id", p.LabOrderLineID))
	}
	_ = msg.Ack()
}
