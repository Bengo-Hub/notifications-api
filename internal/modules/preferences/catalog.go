package preferences

import (
	"sort"
	"strings"
)

// NoChannels is stored as a channel selection when a tenant (or the platform default) switches
// every channel of a type off: config values may not be empty, and "" would read as "no override".
// It matches no channel, so every send of that type is dropped until a channel is switched on.
const NoChannels = "none"

// ChannelsValue is the stored form of a channel selection.
func ChannelsValue(channels []string) string {
	if len(channels) == 0 {
		return NoChannels
	}
	return strings.Join(channels, ",")
}

// notTypes are template files that are not notifications anyone receives.
var notTypes = map[string]bool{"system_test": true}

// Catalog is every notification type a tenant can configure: each registry entry (its class,
// label and group) plus every template id found on disk that the registry does not classify. An
// unclassified template is essential (on by default) and labelled from its id, so a new template is
// configurable the day it ships and nothing depends on remembering to add it to Registry
// (owner, 20 November 2026). Sorted by group, then label.
func Catalog(templateIDs []string) []Type {
	out := make([]Type, 0, len(Registry)+len(templateIDs))
	seen := make(map[string]bool, len(Registry)+len(templateIDs))
	for _, t := range Registry {
		if !seen[t.Key] {
			seen[t.Key] = true
			out = append(out, t)
		}
	}
	for _, id := range templateIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] || notTypes[id] {
			continue
		}
		seen[id] = true
		label, group := describe(id)
		out = append(out, Type{Key: id, Label: label, Group: group, Class: ClassEssential})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// LookupIn finds a type in a catalog.
func LookupIn(catalog []Type, key string) (Type, bool) {
	for _, t := range catalog {
		if t.Key == key {
			return t, true
		}
	}
	return Type{}, false
}

// describe labels a template id: "billing/provider_balance_low" is "Provider balance low" in
// "Billing". An id with no folder goes in "General".
func describe(id string) (label, group string) {
	group = "General"
	name := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		group = words(id[:strings.Index(id, "/")])
		name = id[i+1:]
	}
	return words(name), group
}

// words turns "provider_balance_low" into "Provider balance low".
func words(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(s))
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
