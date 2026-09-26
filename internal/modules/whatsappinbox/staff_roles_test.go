package whatsappinbox

import (
	"slices"
	"testing"
)

// Customer chats are pushed only to staff who can reply. Viewer is every synced user's default
// role (riders and customers included), so it must never receive them.
func TestInboxReplyRolesExcludeViewer(t *testing.T) {
	roles := inboxReplyRoles()
	if slices.Contains(roles, "viewer") {
		t.Fatalf("viewer must not receive customer chat pushes: %v", roles)
	}
	for _, want := range []string{"manager", "admin"} {
		if !slices.Contains(roles, want) {
			t.Fatalf("%s answers customers and must receive inbox pushes: %v", want, roles)
		}
	}
}
