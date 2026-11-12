package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
)

// Customer delivery emails never sent: logistics-api sent no customer, delivery_en_route had no
// template, and estimated_arrival, delivered_at, proof_of_delivery, failure_reason and
// support_link were never filled. Each case renders the real template from the payload
// logistics-api's TaskEventData.toMap now publishes for a task created by pos-api.

const deliveryTenant = "11111111-1111-1111-1111-111111111111"

// deliveryPayload is a task event as logistics-api publishes it for an unlinked customer.
func deliveryPayload(extra map[string]interface{}) map[string]interface{} {
	p := map[string]interface{}{
		"task_id": "7a9d3c1e-5b2f-4c8a-9e6d-1f0b2a3c4d5e", "tracking_code": "TRK8K2QX",
		"order_number": "POS-10442", "source_service": "pos", "external_reference": "pos:10442",
		"customer_email": "amina@example.com", "customer_name": "Amina Wanjiru", "customer_phone": "0712345678",
		"notification": map[string]interface{}{"target": "customer"},
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// renderDelivery runs an event through the consumer's recipient and data steps and renders it.
func renderDelivery(t *testing.T, eventType string, payload map[string]interface{}, ti *tenantInfo) string {
	t.Helper()
	t.Setenv("NOTIFICATIONS_LOGISTICS_APP_URL", "")
	mapping, ok := deliveryMappings["logistics."+eventType]
	if !ok {
		t.Fatalf("no mapping for %s", eventType)
	}
	evt := deliveryEvent{ID: "e1", AggregateType: "logistics", EventType: eventType, TenantID: deliveryTenant, Payload: payload}
	customer, outcome := deliveryCustomer(context.Background(), nil, evt, ti)
	if outcome != noticeSend || customer.Email != "amina@example.com" {
		t.Fatalf("%s: customer %+v outcome %v, want the payload email", eventType, customer, outcome)
	}
	return renderEmail(t, mapping.TemplateID, deliveryEmailData(mapping, payload, ti, customer))
}

// mustNotBlank fails when a greeting or a label renders with nothing after it.
func mustNotBlank(t *testing.T, name, out string) {
	t.Helper()
	for _, blank := range []string{"Hello ,", "Hello  ,", "</strong> </p>", "</strong></p>", `href=""`} {
		if strings.Contains(out, blank) {
			t.Fatalf("%s: blank %q in:\n%s", name, blank, out)
		}
	}
}

func TestDeliveryAssignedEmail(t *testing.T) {
	ti := &tenantInfo{ID: uuid.MustParse(deliveryTenant), ContactEmail: "help@duka.example"}
	out := renderDelivery(t, "task.assigned", deliveryPayload(map[string]interface{}{
		"status": "assigned", "rider_name": "Brian Otieno", "rider_phone": "0722000111",
		"estimated_arrival": "2026-11-12T09:45:00Z",
	}), ti)
	mustContain(t, "assigned", out, "Hello Amina Wanjiru,", "POS-10442", "Brian Otieno",
		"12 Nov 2026, 12:45 PM", "https://logistics.codevertexafrica.com/track/TRK8K2QX")
	mustNotBlank(t, "assigned", out)
	if strings.Contains(out, "7a9d3c1e") {
		t.Fatalf("assigned shows the task UUID:\n%s", out)
	}
}

func TestDeliveryEnRouteEmail(t *testing.T) {
	ti := &tenantInfo{ID: uuid.MustParse(deliveryTenant)}
	// logistics-api puts the active rider on the en_route_dropoff (and legacy en_route) event.
	out := renderDelivery(t, "task.en_route_dropoff", deliveryPayload(map[string]interface{}{
		"status": "en_route_dropoff", "previous_status": "picked_up",
		"rider_name": "Brian Otieno", "rider_phone": "0722000111",
	}), ti)
	mustContain(t, "en_route", out, "Hello Amina Wanjiru,", "on its way", "POS-10442", "/track/TRK8K2QX",
		"Rider:", "Brian Otieno", "Rider Contact:", "0722000111")
	mustNotBlank(t, "en_route", out)

	// No rider resolved and, without a due time, no estimated arrival: those lines are hidden.
	bare := renderDelivery(t, "task.en_route", deliveryPayload(map[string]interface{}{
		"status": "en_route", "previous_status": "picked_up",
	}), ti)
	mustContain(t, "en_route bare", bare, "on its way", "POS-10442")
	mustNotBlank(t, "en_route bare", bare)
	for _, hidden := range []string{"Rider:", "Rider Contact:", "Estimated Arrival:", "your rider"} {
		if strings.Contains(bare, hidden) {
			t.Fatalf("en_route shows %q with no value:\n%s", hidden, bare)
		}
	}
}

// No rider on the event: the assigned email leaves out the Rider line rather than showing a
// stand-in name.
func TestDeliveryAssignedEmailWithoutRider(t *testing.T) {
	out := renderDelivery(t, "task.assigned", deliveryPayload(map[string]interface{}{"status": "assigned"}),
		&tenantInfo{ID: uuid.MustParse(deliveryTenant)})
	mustNotBlank(t, "assigned no rider", out)
	for _, hidden := range []string{"Rider:", "your rider"} {
		if strings.Contains(out, hidden) {
			t.Fatalf("assigned shows %q with no rider:\n%s", hidden, out)
		}
	}
}

// The rider alert on task.assigned goes out on the first delivery only, so a CRM retry of the
// customer email never repeats it. Unreadable metadata counts as the first delivery.
func TestRiderAlertOnlyOnFirstDelivery(t *testing.T) {
	if !isFirstDelivery(&fakeRetryMsg{delivered: 1}) {
		t.Fatal("first delivery must alert the rider")
	}
	if isFirstDelivery(&fakeRetryMsg{delivered: 2}) {
		t.Fatal("a redelivery must not alert the rider again")
	}
	if !isFirstDelivery(&fakeRetryMsg{mdErr: errors.New("no metadata")}) {
		t.Fatal("unknown delivery count must alert the rider")
	}
}

func TestDeliveryCompletedEmail(t *testing.T) {
	ti := &tenantInfo{ID: uuid.MustParse(deliveryTenant)}
	out := renderDelivery(t, "task.completed", deliveryPayload(map[string]interface{}{
		"status": "delivered", "delivered_at": "2026-11-12T09:30:00Z", "delivered_via": "code",
		"code_verified": true, "recipient_name": "Amina", "proof_photo": true,
	}), ti)
	mustContain(t, "completed", out, "Hello Amina Wanjiru,", "12 Nov 2026, 12:30 PM",
		"Confirmed with your delivery code. Received by Amina. Photo taken at the door", "/track/TRK8K2QX")
	mustNotBlank(t, "completed", out)

	// Closed through the status endpoint (no proof of delivery record): task.delivered, same
	// template.
	byHand := renderDelivery(t, "task.delivered", deliveryPayload(map[string]interface{}{
		"status": "delivered", "delivered_at": "2026-11-12T09:30:00Z",
	}), ti)
	mustNotBlank(t, "delivered", byHand)
	if strings.Contains(byHand, "Proof of Delivery:") {
		t.Fatalf("delivered by hand has no proof to show:\n%s", byHand)
	}
}

func TestDeliveryFailedEmail(t *testing.T) {
	payload := deliveryPayload(map[string]interface{}{
		"status": "failed", "reason": "Gate locked, customer not answering", "failure_reason": "Gate locked, customer not answering",
	})
	out := renderDelivery(t, "task.failed", payload, &tenantInfo{ID: uuid.MustParse(deliveryTenant), ContactEmail: "help@duka.example"})
	mustContain(t, "failed", out, "Hello Amina Wanjiru,", "POS-10442", "Gate locked, customer not answering",
		`href="mailto:help@duka.example"`)
	mustNotBlank(t, "failed", out)

	// No support email: the phone is shown as text, never as a button.
	phoneOnly := renderDelivery(t, "task.failed", payload, &tenantInfo{ID: uuid.MustParse(deliveryTenant), ContactPhone: "0700 123 456"})
	mustContain(t, "failed phone", phoneOnly, "0700 123 456")
	if strings.Contains(phoneOnly, "Contact Support") {
		t.Fatalf("no support email, so no button:\n%s", phoneOnly)
	}

	// No support contact at all: no button and no blank line.
	none := renderDelivery(t, "task.failed", payload, &tenantInfo{ID: uuid.MustParse(deliveryTenant)})
	mustNotBlank(t, "failed none", none)
	if strings.Contains(none, "Contact Support") || strings.Contains(none, "Call us") {
		t.Fatalf("no support contact, so nothing to show:\n%s", none)
	}
}

// A customer with no name in the event is greeted "there", not "Hello ,".
func TestDeliveryEmailGreetsThereWithoutName(t *testing.T) {
	payload := deliveryPayload(map[string]interface{}{"status": "assigned"})
	delete(payload, "customer_name")
	out := renderDelivery(t, "task.assigned", payload, &tenantInfo{ID: uuid.MustParse(deliveryTenant)})
	mustContain(t, "no name", out, "Hello there,")
	mustNotBlank(t, "no name", out)
}

// logistics-api sends crm_contact_id alone for a CRM-linked customer: the email goes to the CRM
// address and greets the CRM name. With the CRM down it is retried; when the CRM has no email
// for the contact it is dropped, never sent to the business instead.
func TestDeliveryCustomerFromCRM(t *testing.T) {
	tenant := uuid.MustParse(deliveryTenant)
	contactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ti := &tenantInfo{ID: tenant, ContactEmail: "help@duka.example"}
	evt := deliveryEvent{ID: "e2", AggregateType: "logistics", EventType: "task.assigned", TenantID: tenant.String(),
		Payload: map[string]interface{}{"task_id": "t1", "tracking_code": "TRK1", "crm_contact_id": contactID.String()}}

	res := crmServer(t, &crm.Contact{ID: contactID, FirstName: "Amina", LastName: "Wanjiru", Email: "amina@crm.example"})
	got, outcome := deliveryCustomer(context.Background(), res, evt, ti)
	if outcome != noticeSend || got.Email != "amina@crm.example" || got.Name != "Amina Wanjiru" {
		t.Fatalf("got %+v outcome %v, want the CRM contact", got, outcome)
	}

	if _, outcome := deliveryCustomer(context.Background(), crmServer(t, nil), evt, ti); outcome != noticeRetry {
		t.Fatalf("outcome %v, want a retry while the CRM is down", outcome)
	}

	noEmail := crmServer(t, &crm.Contact{ID: contactID, FirstName: "Amina"})
	if got, outcome := deliveryCustomer(context.Background(), noEmail, evt, ti); outcome != noticeDrop || got.Email != "" {
		t.Fatalf("got %+v outcome %v, want the notice dropped", got, outcome)
	}
}

// An event with no customer at all sends nothing (it used to mail the business).
func TestDeliveryWithoutCustomerSendsNothing(t *testing.T) {
	evt := deliveryEvent{ID: "e3", AggregateType: "logistics", EventType: "task.assigned", TenantID: deliveryTenant,
		Payload: map[string]interface{}{"task_id": "t1", "tracking_code": "TRK1"}}
	got, outcome := deliveryCustomer(context.Background(), nil, evt, &tenantInfo{ContactEmail: "help@duka.example"})
	if outcome != noticeSend || got.Email != "" {
		t.Fatalf("got %+v outcome %v, want no recipient", got, outcome)
	}
}
