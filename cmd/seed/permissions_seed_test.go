package main

import (
	"os"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/modules/identity"
)

// Every permission a role gets by default must be seeded, or the role's permission refresh fails
// on the foreign key and the role silently keeps stale permissions.
func TestEveryDefaultPermissionIsSeeded(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range identity.AllPermissions() {
		if !strings.Contains(string(src), `{"`+string(p)+`",`) {
			t.Errorf("permission %s is used by roles but not seeded in seedPermissions", p)
		}
	}
}
