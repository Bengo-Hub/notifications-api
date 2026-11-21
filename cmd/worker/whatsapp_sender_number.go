package main

import "github.com/bengobox/notifications-api/internal/messaging"

// whatsAppNumberTenant picks whose WhatsApp number a message goes from. Sender scope decides who
// pays and whether a plan can block the send; it does not decide the number. A tenant's messages to
// its own customers or staff (sign-in codes included, which are platform scope so no plan ever
// blocks a login) always go from that tenant's own number, never from Codevertex's (owner, 9 and
// 20 November 2026). Only the demo allowlist may borrow the platform number, which
// providers.Manager.GetWhatsAppProvider enforces. Codevertex's own notices to a tenant's admins
// (billing, plan expiry, credits) and to platform admins stay on the platform number.
func whatsAppNumberTenant(msg *messaging.Message, resolvedTenantID, platformID string) string {
	if msg.EffectiveSenderScope() != messaging.SenderScopePlatform {
		return resolvedTenantID
	}
	if resolvedTenantID == "" || resolvedTenantID == platformID {
		return platformID
	}
	switch msg.Target {
	case messaging.TargetTenantAdmin, messaging.TargetPlatformAdmin:
		return platformID
	default:
		return resolvedTenantID
	}
}
