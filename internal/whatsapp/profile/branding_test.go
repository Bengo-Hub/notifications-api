package profile

import (
	"testing"

	cache "github.com/Bengo-Hub/cache"
)

// Branding comes from auth-api's tenant record: name, logo, contact email and website are fields,
// the tagline, description and address are tenant metadata.
func TestBrandingFromDetails(t *testing.T) {
	b := brandingFromDetails(cache.TenantDetails{
		Name: "Hadia", LogoURL: "https://cdn.example/hadia.png", ContactEmail: "hello@hadia.example", Website: "hadia.example",
		Metadata: map[string]any{"tagline": " Fresh food, fast ", "description": "Home cooked meals", "address": "Moi Ave, Nairobi", "other": 3},
	})
	want := Branding{Name: "Hadia", About: "Fresh food, fast", Description: "Home cooked meals", Address: "Moi Ave, Nairobi",
		Email: "hello@hadia.example", Website: "hadia.example", LogoURL: "https://cdn.example/hadia.png"}
	if b != want {
		t.Fatalf("got %+v\nwant %+v", b, want)
	}
}

func TestBrandingFromDetailsIgnoresNonStringMetadata(t *testing.T) {
	b := brandingFromDetails(cache.TenantDetails{Name: "X", Metadata: map[string]any{"tagline": 5, "address": map[string]any{"city": "Nairobi"}}})
	if b.About != "" || b.Address != "" {
		t.Fatalf("got %+v", b)
	}
}
