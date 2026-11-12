package whatsapp

import (
	"os"
	"strings"
)

// defaultGraphVersion is used when neither the provider settings nor META_GRAPH_API_VERSION name
// a Graph API version. Keep it in step with the version on Platform > Providers > WhatsApp.
const defaultGraphVersion = "v26.0"

// GraphVersion is the Graph API version for a Meta call: the configured one (the meta_cloud
// "api_version" setting) when set, else META_GRAPH_API_VERSION, else defaultGraphVersion. One rule
// for sending, Embedded Signup and template sync, which used to fall back to v20.0, v21.0 and
// v25.0 respectively.
func GraphVersion(configured string) string {
	if v := strings.TrimSpace(configured); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("META_GRAPH_API_VERSION")); v != "" {
		return v
	}
	return defaultGraphVersion
}
