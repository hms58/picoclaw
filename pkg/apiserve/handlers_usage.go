package apiserve

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// handleUsage aggregates usage records for the authenticated key.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing principal", r)
		return
	}

	// The key filter is always applied: one key must never see another's usage.
	query := UsageQuery{APIKeyID: principal.APIKeyID}

	groupBy := strings.TrimSpace(r.URL.Query().Get("group_by"))
	switch strings.ToLower(groupBy) {
	case "", "agent", "api_key", "user":
		query.GroupBy = strings.ToLower(groupBy)
	default:
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"unsupported group_by: "+groupBy, r)
		return
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		ts, err := parseUnixOrRFC3339(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid from: "+raw, r)
			return
		}
		query.From = ts
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		ts, err := parseUnixOrRFC3339(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid to: "+raw, r)
			return
		}
		query.To = ts
	}
	if agentID := strings.TrimSpace(r.URL.Query().Get("agent_id")); agentID != "" {
		if !principal.AllowsAgent(normalizeAgentID(agentID)) {
			writeError(w, http.StatusForbidden, codeAgentNotAllowed, "api key may not use "+agentID, r)
			return
		}
		query.AgentID = normalizeAgentID(agentID)
	}

	rows, err := s.store.SumUsage(r.Context(), query)
	if err != nil {
		// group_by is validated above, so reaching here means a storage failure,
		// not bad input. Report it as a server error and do not echo the driver's
		// message (it can carry table and constraint names).
		logger.ErrorCF("apiserve", "usage query failed", map[string]any{
			"api_key_id": principal.APIKeyID,
			"error":      err.Error(),
		})
		writeError(w, http.StatusInternalServerError, "internal_error", "usage query failed", r)
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"key":               row.Key,
			"requests":          row.Requests,
			"prompt_tokens":     row.PromptTokens,
			"completion_tokens": row.CompletionTokens,
			"total_tokens":      row.TotalTokens,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"group_by": query.GroupBy,
		"rows":     out,
	})
}

// parseUnixOrRFC3339 accepts epoch seconds or an RFC3339 timestamp.
func parseUnixOrRFC3339(raw string) (time.Time, error) {
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(seconds, 0), nil
	}
	return time.Parse(time.RFC3339, raw)
}
