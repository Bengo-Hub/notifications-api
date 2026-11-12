package whatsapp

import "testing"

func TestGraphVersion(t *testing.T) {
	t.Setenv("META_GRAPH_API_VERSION", "")
	if got := GraphVersion(""); got != "v26.0" {
		t.Errorf("default = %q, want v26.0", got)
	}
	if got := GraphVersion(" v27.0 "); got != "v27.0" {
		t.Errorf("configured = %q, want v27.0", got)
	}
	t.Setenv("META_GRAPH_API_VERSION", "v25.0")
	if got := GraphVersion(""); got != "v25.0" {
		t.Errorf("env = %q, want v25.0", got)
	}
	if got := GraphVersion("v26.0"); got != "v26.0" {
		t.Errorf("setting beats env: got %q", got)
	}
}
