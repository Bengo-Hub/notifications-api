package handlers

import (
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

func TestFilterSpecToTags_KeepsOnlyAllowedTaggedOperations(t *testing.T) {
	spec := map[string]any{
		"swagger": "2.0",
		"paths": map[string]any{
			"/{tenantId}/notifications/messages": map[string]any{
				"post": map[string]any{"tags": []any{"Notifications"}},
			},
			"/platform/providers": map[string]any{
				"get":  map[string]any{"tags": []any{"Platform"}},
				"post": map[string]any{"tags": []any{"Platform"}},
			},
			"/healthz": map[string]any{
				"get": map[string]any{"tags": []any{"Health"}},
			},
		},
		"definitions": map[string]any{"Foo": map[string]any{}},
	}
	allowed := map[string]bool{"Notifications": true, "Health": true}

	filtered := filterSpecToTags(spec, allowed)

	paths, ok := filtered["paths"].(map[string]any)
	if !ok {
		t.Fatalf("expected paths to be a map, got %T", filtered["paths"])
	}
	if _, ok := paths["/{tenantId}/notifications/messages"]; !ok {
		t.Error("expected the Notifications path to survive filtering")
	}
	if _, ok := paths["/healthz"]; !ok {
		t.Error("expected the Health path to survive filtering")
	}
	if _, ok := paths["/platform/providers"]; ok {
		t.Error("expected the internal Platform path to be filtered out")
	}
	if len(paths) != 2 {
		t.Errorf("expected exactly 2 surviving paths, got %d", len(paths))
	}
	if _, ok := filtered["definitions"]; !ok {
		t.Error("expected non-paths keys like definitions to pass through untouched")
	}
}

func TestFilterSpecToTags_DropsOperationsWithNoTags(t *testing.T) {
	spec := map[string]any{
		"paths": map[string]any{
			"/api/v1/untagged": map[string]any{
				"get": map[string]any{},
			},
		},
	}
	filtered := filterSpecToTags(spec, map[string]bool{"Health": true})
	paths := filtered["paths"].(map[string]any)
	if len(paths) != 0 {
		t.Errorf("expected an untagged operation to be dropped, got %d paths", len(paths))
	}
}

func TestFilterSpecToTags_MultiMethodPathKeepsOnlyAllowedMethod(t *testing.T) {
	spec := map[string]any{
		"paths": map[string]any{
			"/platform/providers/{id}": map[string]any{
				"patch":  map[string]any{"tags": []any{"Platform"}},
				"delete": map[string]any{"tags": []any{"Platform"}},
			},
		},
	}
	filtered := filterSpecToTags(spec, map[string]bool{"Notifications": true})
	paths := filtered["paths"].(map[string]any)
	if _, ok := paths["/platform/providers/{id}"]; ok {
		t.Error("expected the internal-only path to be dropped entirely when no method matches")
	}
}

func TestIsPrivilegedForInternalDocs(t *testing.T) {
	cases := []struct {
		name   string
		result *authclient.APIKeyValidationResult
		want   bool
	}{
		{"nil result (anonymous / invalid secret)", nil, false},
		{"platform App secret", &authclient.APIKeyValidationResult{Roles: []string{"superuser", "service"}, Service: "platform"}, true},
		{"tenant App secret, production", &authclient.APIKeyValidationResult{Roles: []string{"service"}, Service: "tenant", Environment: "production"}, false},
		{"tenant App secret, sandbox", &authclient.APIKeyValidationResult{Roles: []string{"service"}, Service: "tenant", Environment: "sandbox"}, false},
		{"no roles at all", &authclient.APIKeyValidationResult{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPrivilegedForInternalDocs(tc.result); got != tc.want {
				t.Errorf("isPrivilegedForInternalDocs(%+v) = %v, want %v", tc.result, got, tc.want)
			}
		})
	}
}
