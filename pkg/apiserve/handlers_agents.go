package apiserve

import "net/http"

// handleAgents lists the agents this API key may use, plus the default one.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing principal", r)
		return
	}

	registry := s.loop.GetRegistry()
	if registry == nil {
		writeJSON(w, http.StatusOK, map[string]any{"agents": []string{}})
		return
	}

	ids := registry.ListAgentIDs()
	visible := make([]string, 0, len(ids))
	for _, id := range ids {
		normalized := normalizeAgentID(id)
		if principal.AllowsAgent(normalized) {
			visible = append(visible, normalized)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"agents": visible})
}
