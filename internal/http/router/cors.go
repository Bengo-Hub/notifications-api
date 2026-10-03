package router

import (
	"net/http"
	"net/url"
	"strings"
)

// platformDomain is the platform's own domain: every app it serves (pos, inventory, logistics,
// isp-billing, ...) lives on a subdomain of it and calls notifications from the browser (branding,
// push config, announcements).
const platformDomain = "codevertexafrica.com"

// originAllower allows the configured origins (custom client domains, localhost) plus any https
// subdomain of the platform domain. HTTP_ALLOWED_ORIGINS used to be the only source, and it
// listed a handful of apps: pos, inventory, logistics and others were refused, so e.g. their
// branding fetch failed its preflight and silently fell back to the default logo and colors.
func originAllower(configured []string) func(r *http.Request, origin string) bool {
	allowed := make(map[string]bool, len(configured))
	wildcard := false
	for _, o := range configured {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "*" {
			wildcard = true
		}
		if o != "" {
			allowed[strings.ToLower(o)] = true
		}
	}
	return func(_ *http.Request, origin string) bool {
		if wildcard || allowed[strings.ToLower(origin)] {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Port() != "" {
			return false
		}
		host := strings.ToLower(u.Hostname())
		return host == platformDomain || strings.HasSuffix(host, "."+platformDomain)
	}
}
