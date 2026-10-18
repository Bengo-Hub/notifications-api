package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// Vehicle insurance alerts from logistics-api's insurance monitor
// (logistics.fleet.vehicle_insurance_expiring / _expired). Each goes by email and WhatsApp to the
// rider on the vehicle (when there is one) and to the business contact. Links are template
// buttons: the rider opens the rider app's profile, where the policy is renewed; the business
// opens the Vehicles page of the logistics app.

const (
	subjectInsuranceExpiring = "logistics.fleet.vehicle_insurance_expiring"
	subjectInsuranceExpired  = "logistics.fleet.vehicle_insurance_expired"

	// Button domains fixed in the logistics_insurance_* templates (docs/whatsapp-template-policy.md).
	riderAppButtonBase     = "https://riderapp.codevertexafrica.com"
	logisticsAppButtonBase = "https://logistics.codevertexafrica.com"
)

// insuranceAudience is one recipient group of an insurance alert.
type insuranceAudience struct {
	key       string // "rider" | "business"
	target    string
	email     string
	phone     string
	path      string // page under /{slug}/ the button and email link open
	appBase   string // button domain fixed in the template
	appEnvKey string // per-environment override of the app address for the email link
	// appService is the tenant service URL key for the email link ("" = none: a tenant's own
	// logistics URL is the admin app, never the rider app).
	appService string
}

// isVehicleInsuranceEvent reports whether the fleet consumer should hand the event here.
func isVehicleInsuranceEvent(subject string) bool {
	return subject == subjectInsuranceExpiring || subject == subjectInsuranceExpired
}

// insuranceDate renders the payload's YYYY-MM-DD as "24 Mar 2027".
func insuranceDate(p map[string]any) string {
	raw, _ := p["expires_on"].(string)
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.Format("2 Jan 2006")
	}
	return raw
}

// insuranceStatus is the short status the expired templates carry as a parameter.
func insuranceStatus(blocked bool) string {
	if blocked {
		return "off delivery jobs until renewed"
	}
	return "still on jobs, please renew"
}

// insuranceData is the template data both channels share.
func insuranceData(p map[string]any, ti *tenantInfo) map[string]any {
	rider, _ := p["rider_name"].(string)
	riderLabel := rider
	if strings.TrimSpace(riderLabel) == "" {
		riderLabel = "no rider linked"
	}
	blocked, _ := p["blocked"].(bool)
	days := 0
	switch v := p["days_left"].(type) {
	case float64:
		days = int(v)
	case int:
		days = v
	}
	business := ""
	if ti != nil {
		business = ti.Name
	}
	return map[string]any{
		"name":          firstWord(rider, "there"),
		"rider_name":    riderLabel,
		"business_name": business,
		"license_plate": mStr(p, "license_plate"),
		"policy_no":     mStr(p, "policy_no"),
		"insurer":       mStr(p, "insurer"),
		"expires_on":    insuranceDate(p),
		"days_left":     days,
		"blocked":       blocked,
		"status":        insuranceStatus(blocked),
	}
}

// firstWord returns the first word of a full name, or fallback.
func firstWord(name, fallback string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return fallback
}

// insuranceWhatsApp returns the template and its parameters for an audience and event.
func insuranceWhatsApp(expired bool, audience string, d map[string]any) (string, []string) {
	s := func(k string) string { return fmt.Sprint(d[k]) }
	switch {
	case !expired && audience == "rider":
		return "logistics_insurance_expiring_rider_v1_btn", []string{s("name"), s("license_plate"), s("expires_on")}
	case !expired:
		return "logistics_insurance_expiring_business_v1_btn", []string{s("business_name"), s("license_plate"), s("rider_name"), s("expires_on")}
	case audience == "rider":
		return "logistics_insurance_expired_rider_v1_btn", []string{s("name"), s("license_plate"), s("expires_on"), s("status")}
	default:
		return "logistics_insurance_expired_business_v1_btn", []string{s("business_name"), s("license_plate"), s("rider_name"), s("expires_on"), s("status")}
	}
}

// dispatchVehicleInsurance sends the alert to the rider and the business as one notice.
func dispatchVehicleInsurance(ctx context.Context, nc *nats.Conn, cfg *config.Config, ti *tenantInfo, tenantID, eventID, subject string, p map[string]any) error {
	expired := subject == subjectInsuranceExpired
	event := "expiring"
	if expired {
		event = "expired"
	}
	data := insuranceData(p, ti)
	slug, _ := p["tenant_slug"].(string)
	country := ""
	if ti != nil {
		country = ti.Country
		if ti.Slug != "" {
			slug = ti.Slug
		}
	}

	audiences := []insuranceAudience{{
		key: "rider", target: messaging.TargetRider,
		email: mStr(p, "rider_email"), phone: mStr(p, "rider_phone"),
		path: "profile", appBase: riderAppButtonBase, appEnvKey: "NOTIFICATIONS_RIDER_APP_URL",
	}}
	if ti != nil {
		audiences = append(audiences, insuranceAudience{
			key: "business", target: messaging.TargetTenantAdmin,
			email: ti.ContactEmail, phone: ti.ContactPhone,
			path: "vehicles", appBase: logisticsAppButtonBase, appEnvKey: "NOTIFICATIONS_LOGISTICS_APP_URL", appService: "logistics",
		})
	}

	parties := make([]noticeParty, 0, len(audiences))
	for _, a := range audiences {
		own := map[string]any{"audience": a.key}
		suffix := ""
		if slug != "" {
			suffix = slug + "/" + a.path
			app := a.appBase
			if ti != nil {
				app = ti.ServiceURL(a.appService, a.appEnvKey, a.appBase)
			}
			own["action_link"] = strings.TrimRight(app, "/") + "/" + suffix
		}
		waName, waParams := insuranceWhatsApp(expired, a.key, data)
		parties = append(parties, noticeParty{
			Key:          a.key,
			Target:       a.target,
			Email:        a.email,
			Phone:        a.phone,
			WATemplateID: "logistics/vehicle_insurance_" + event + "_" + a.key,
			Data:         own,
			WhatsApp:     &partyWhatsApp{Name: waName, Params: waParams, Button: suffix},
		})
	}

	mailSubject := fmt.Sprintf("Vehicle %s insurance expires on %s", data["license_plate"], data["expires_on"])
	if expired {
		mailSubject = fmt.Sprintf("Vehicle %s insurance has expired", data["license_plate"])
	}
	return sendToParties(ctx, nc, cfg, partyNotice{
		ServiceID:  "logistics",
		TenantID:   tenantID,
		TemplateID: "logistics/vehicle_insurance_" + event,
		Subject:    mailSubject,
		Data:       data,
		Country:    country,
		IdemBase:   "logistics-insurance-" + eventID,
		Parties:    parties,
	})
}
