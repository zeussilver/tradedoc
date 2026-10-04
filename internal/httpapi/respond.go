package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

const maxJSONBody = 1 << 20 // 1 MiB

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
