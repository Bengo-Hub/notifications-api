package main

import (
	"os"
	"strings"
)

// serviceURL returns the URL from the given env var, or falls back to the tenant website.
// This ensures email links point to the correct service frontend (ordering app, POS, etc.)
// rather than the tenant's public website.
func serviceURL(envVar, fallback string) string {
	if u := os.Getenv(envVar); u != "" {
		return strings.TrimRight(u, "/")
	}
	return fallback
}

// serviceURLWithSlug returns the service URL with tenant slug appended.
func serviceURLWithSlug(envVar, tenantSlug, fallback string) string {
	base := serviceURL(envVar, fallback)
	if tenantSlug != "" && !strings.HasSuffix(base, "/"+tenantSlug) {
		return base + "/" + tenantSlug
	}
	return base
}
