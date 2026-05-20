package web

import (
	"encoding/json"
	"net/http"
)

// errorBody is the canonical JSON shape returned for HTTP errors.
type errorBody struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

// writeJSON serializes v as JSON with the given status. If encoding fails the
// connection is dropped — there is nothing useful left to send.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an errorBody response with the given HTTP status. The
// short error string mirrors http.StatusText for the status code so callers
// only need to supply a human-friendly message.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody{
		Error:   http.StatusText(status),
		Code:    status,
		Message: message,
	})
}
