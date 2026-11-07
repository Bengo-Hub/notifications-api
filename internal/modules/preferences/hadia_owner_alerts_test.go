package preferences

import "testing"

// Hadia's owner alerts are on unless a tenant switches them off: each owner already chooses one
// message per gift or a daily summary in Hadia, and hadia-api sends only the one chosen. When they
// were optional (off by default) every gift alert was dropped on staging, whatever the owner chose
// (8 November 2026).
func TestHadiaOwnerAlertsDefaultOn(t *testing.T) {
	for _, id := range []string{"hadia/gift_owner_alert", "hadia/owner_gift_digest", "hadia/reservation_owner_alert", "hadia/reservation_purchased_owner"} {
		if !DefaultEnabled(id) {
			t.Errorf("%s is off by default; owners would never hear about their gifts", id)
		}
	}
}
