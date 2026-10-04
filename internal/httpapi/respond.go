package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// maxJSONBody caps every JSON request body. The biggest one is a manual
// extraction (a whole invoice typed in). 256 KiB is far more than a real
// invoice needs, and keeps the stored reading inside DynamoDB's 400 KB item
// limit; anything that still ends up too big to store gets 413 too_large
// (see storeError) instead of a 500.
const maxJSONBody = 256 << 10 // 256 KiB

// Every error response has the same shape, so the frontend handles one format:
// {"error": {"code": "...", "message": "...", "details": [...]}}
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string, details any) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg, Details: details}})
}

// decodeJSON reads a size-limited body and rejects unknown fields, so a typo
// like "refernce" is an error instead of being silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large", nil)
		case errors.Is(err, io.EOF):
			writeError(w, http.StatusBadRequest, "empty_body", "request body is empty", nil)
		default:
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		}
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid_json", "body must contain a single JSON object", nil)
		return false
	}
	return true
}

// orEmpty turns a nil slice into an empty one, so lists are sent as []
// rather than null and the frontend never has to check for both.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
