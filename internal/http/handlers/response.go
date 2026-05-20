package handlers

import (
	"encoding/json"
	"net/http"
)

// respondJSON writes the provided payload as a JSON response with the given status code.
// This is the canonical response helper for all handlers in this package.
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// jsonResponse is an alias for respondJSON for compatibility with platform_providers.go callers.
func jsonResponse(w http.ResponseWriter, status int, payload any) {
	respondJSON(w, status, payload)
}

// jsonError writes a JSON error response using the {"error":"..."} shape.
func jsonError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}

// RespondError is the exported version of jsonError, used by the identity sub-package.
func RespondError(w http.ResponseWriter, status int, message string) {
	jsonError(w, status, message)
}
