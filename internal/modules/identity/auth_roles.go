package identity

import (
	"sort"
	"strings"
)

// MetaAuthRoles is the users.metadata key holding the roles auth-api gave the person in this
// tenant, as it sent them. The local Role enum is lossy (every cashier, waiter or driver collapses
// to viewer or to nothing), so the email-verification gate reads this instead to tell a staff
// account from a customer.
const MetaAuthRoles = "auth_roles"

// customerFacingRoles are the roles a person can take on without an invite: a storefront
// customer, a learner, a learner's parent, a rider applicant, or a plain organisation member.
// It mirrors auth-api's selfServiceRoles; a role outside this list means the person was added to
// the tenant on purpose (staff, owner, admin).
var customerFacingRoles = []string{"customer", "driver", "member", "parent", "student"}

// CustomerFacingRoles returns the self-service roles as a fresh slice the caller may keep.
func CustomerFacingRoles() []string {
	return append([]string(nil), customerFacingRoles...)
}

// rawAuthRoles reads the roles array of an auth event (a []string from the typed created event,
// []interface{} from a decoded payload), lower-cased, de-duplicated and sorted.
func rawAuthRoles(authUserData map[string]interface{}) []string {
	var in []string
	switch v := authUserData["roles"].(type) {
	case []string:
		in = v
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				in = append(in, s)
			}
		}
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// withAuthRoles returns metadata with the event's roles recorded. An event that carries no roles
// leaves what is already recorded alone, so a profile update never erases the role history.
func withAuthRoles(metadata map[string]interface{}, authUserData map[string]interface{}) map[string]interface{} {
	roles := rawAuthRoles(authUserData)
	if len(roles) == 0 {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]interface{}, 1)
	}
	metadata[MetaAuthRoles] = roles
	return metadata
}
