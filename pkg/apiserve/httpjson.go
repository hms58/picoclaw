package apiserve

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// writeJSON writes v as a JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the canonical error envelope.
func writeError(w http.ResponseWriter, status int, code, message string, r *http.Request) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":       code,
			"message":    message,
			"request_id": requestIDFrom(r),
		},
	})
}

// requestIDFrom returns the caller supplied request id or a fresh random one.
func requestIDFrom(r *http.Request) string {
	if r != nil {
		if id := r.Header.Get("X-Request-Id"); id != "" {
			return id
		}
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}
