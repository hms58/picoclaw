package apiserve

import "net/http"

// registerRoutes binds every endpoint to the ServeMux.
// /health and /ready stay unauthenticated; everything under /v1 goes through
// the access log and then authentication + rate limiting.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /ready", s.handleReady)

	guarded := func(h http.HandlerFunc) http.Handler {
		return s.withAccessLog(s.withMiddleware(h))
	}

	s.mux.Handle("GET /v1/agents", guarded(s.handleAgents))
	s.mux.Handle("POST /v1/messages", guarded(s.handleMessages))
	s.mux.Handle("POST /v1/tasks", guarded(s.handleCreateTask))
	s.mux.Handle("GET /v1/tasks/{task_id}", guarded(s.handleGetTask))
	s.mux.Handle("POST /v1/tasks/{task_id}/cancel", guarded(s.handleCancelTask))
	s.mux.Handle("GET /v1/usage", guarded(s.handleUsage))
}
