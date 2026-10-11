package apiserve

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// idempotencyScopeOf namespaces replay records by the calling API key.
//
// Every /v1 route is wrapped in withMiddleware, so the principal is always on
// the context. Scoping by it is what stops the caller-supplied Idempotency-Key
// from being a global namespace shared with other keys.
func idempotencyScopeOf(ctx context.Context) string {
	principal, ok := PrincipalFrom(ctx)
	if !ok {
		return ""
	}
	return principal.APIKeyID
}

// replayIdempotent writes the stored response for key and reports whether it did.
//
// Both the sync (200) and async (202) surfaces share this: only the status code
// differs, and duplicating the logic is how the two copies drift apart.
func (s *Server) replayIdempotent(
	w http.ResponseWriter,
	r *http.Request,
	key string,
	status int,
) bool {
	body, found, err := s.store.GetIdempotentResponse(
		r.Context(), idempotencyScopeOf(r.Context()), key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), r)
		return true
	}
	if !found {
		return false
	}

	// Re-encode through writeJSON so the replayed body is byte-identical to a
	// fresh response (json.Encoder appends a newline that Marshal does not).
	w.Header().Set("Idempotent-Replay", "true")
	writeJSON(w, status, json.RawMessage(body))
	return true
}

// storeIdempotent captures a response for replay. A failure is logged, never
// returned: losing a replay record must not fail a request that already
// succeeded.
func (s *Server) storeIdempotent(ctx context.Context, key string, v any) {
	if key == "" {
		return
	}

	body, err := json.Marshal(v)
	if err != nil {
		logger.WarnCF("apiserve", "failed to encode idempotent response",
			map[string]any{"error": err.Error()})
		return
	}
	if err := s.store.SaveIdempotentResponse(ctx, idempotencyScopeOf(ctx), key, body); err != nil {
		logger.WarnCF("apiserve", "failed to persist idempotent response",
			map[string]any{"error": err.Error()})
	}
}
