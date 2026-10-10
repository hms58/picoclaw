package apiserve

import (
	"context"
	"net/http"
	"strings"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// Principal is the authenticated caller of one request.
type Principal struct {
	APIKeyID      string
	Name          string
	TenantID      string
	AllowedAgents []string
	AllowedSkills []string
	RateLimit     string
}

// AllowsAgent reports whether this principal may use the given (already
// normalized) agent id. An empty AllowedAgents list means unrestricted.
func (p *Principal) AllowsAgent(agentID string) bool {
	if p == nil {
		return false
	}
	if len(p.AllowedAgents) == 0 {
		return true
	}
	for _, allowed := range p.AllowedAgents {
		if normalizeAgentID(allowed) == agentID {
			return true
		}
	}
	return false
}

type principalCtxKey struct{}

// WithPrincipal stores the authenticated principal on the context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFrom extracts the authenticated principal.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok
}

// withMiddleware authenticates the bearer token, applies the key's rate limit,
// and injects a Principal.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing bearer token", r)
			return
		}

		key, err := s.store.LookupAPIKey(r.Context(), token)
		if err != nil {
			// Unknown and disabled keys are indistinguishable to callers.
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "invalid api key", r)
			return
		}

		if !s.limiter.Allow(key.ID, key.RateLimit) {
			writeError(w, http.StatusTooManyRequests, codeRateLimited, "rate limit exceeded", r)
			return
		}

		if touchErr := s.store.TouchAPIKey(r.Context(), key.ID); touchErr != nil {
			logger.WarnCF("apiserve", "failed to touch api key", map[string]any{"error": touchErr.Error()})
		}

		principal := &Principal{
			APIKeyID:      key.ID,
			Name:          key.Name,
			TenantID:      key.TenantID,
			AllowedAgents: key.AllowedAgents,
			AllowedSkills: key.AllowedSkills,
			RateLimit:     key.RateLimit,
		}
		// Enrich the access record with the key dimension. The record is a
		// pointer on the request context (see accesslog.go), so mutating it here
		// is visible to the outer middleware, which is where it gets emitted.
		if rec, ok := accessRecordFrom(r.Context()); ok {
			rec.APIKeyID = key.ID
		}

		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
	})
}

// bearerToken extracts the token from the Authorization header.
func bearerToken(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(raw[len(prefix):])
}

// resolveAgent normalizes the requested id and looks it up in the registry.
// It returns the normalized id so callers log and persist a canonical value.
func (s *Server) resolveAgent(id string) (*agent.AgentInstance, string, bool) {
	normalized := normalizeAgentID(id)
	registry := s.loop.GetRegistry()
	if registry == nil {
		return nil, normalized, false
	}
	instance, ok := registry.GetAgent(normalized)
	if !ok {
		return nil, normalized, false
	}
	return instance, normalized, true
}
