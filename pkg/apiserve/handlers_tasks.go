package apiserve

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// handleCreateTask enqueues an asynchronous agent turn.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing principal", r)
		return
	}

	// Short-circuit before decoding, mirroring handleMessages: a retry carrying a
	// now-invalid body must still replay rather than answer 400.
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if s.replayIdempotent(w, r, idemKey, http.StatusAccepted) {
		return
	}

	var req messagesRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err := decoder.Decode(&req); err != nil {
		// Same 413 handling as handleMessages: MaxBytesReader writes the status
		// itself, so a 400 here would be a wasted second WriteHeader.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, codeInvalidRequest,
				"request body exceeds 1 MiB", r)
			return
		}
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid json body", r)
		return
	}

	if len(req.Skills) > 0 || len(req.ToolsAllow) > 0 {
		writeError(w, http.StatusBadRequest, codeCapabilityMissing,
			"skills[] and tools_allow[] need the ProcessDirectWithOptions patch (P1)", r)
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "agent_id is required", r)
		return
	}

	_, agentID, found := s.resolveAgent(req.AgentID)
	if !found {
		writeError(w, http.StatusNotFound, codeAgentNotFound, "unknown agent_id: "+req.AgentID, r)
		return
	}
	if !principal.AllowsAgent(agentID) {
		writeError(w, http.StatusForbidden, codeAgentNotAllowed, "api key may not use "+agentID, r)
		return
	}

	message := strings.TrimSpace(req.Message)
	if message == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "message is required", r)
		return
	}

	prepared, ok := s.prepareMessage(w, r, principal, &req, message)
	if !ok {
		return
	}
	message = prepared

	conv, err := s.ensureConversation(r.Context(), principal, agentID, req.UserID, req.ConversationID)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, codeConversationGone, "unknown conversation_id", r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), r)
		return
	}

	// §7.3 is per-request, not per-message: without this the async routes would
	// never log the agent or conversation, which is exactly where a log reader
	// cannot recover them from a response body.
	markAccessRecord(r, agentID, conv.ConversationID)

	req.AgentID = agentID
	req.Message = message

	task, err := s.store.CreateTask(r.Context(), CreateTaskParams{
		APIKeyID:       principal.APIKeyID,
		AgentID:        agentID,
		UserID:         conv.UserID,
		ConversationID: conv.ConversationID,
		SessionKey:     conv.SessionKey,
		Request:        req,
		CallbackURL:    strings.TrimSpace(req.CallbackURL),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), r)
		return
	}

	payload := map[string]string{
		"task_id":         task.ID,
		"status":          task.Status,
		"conversation_id": task.ConversationID,
	}
	s.storeIdempotent(r.Context(), idemKey, payload)

	writeJSON(w, http.StatusAccepted, payload)
}

// handleGetTask returns a task, including its result once finished.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing principal", r)
		return
	}

	taskID := r.PathValue("task_id")
	task, err := s.store.GetTask(r.Context(), taskID)
	if errors.Is(err, ErrNotFound) || (err == nil && task.APIKeyID != principal.APIKeyID) {
		writeError(w, http.StatusNotFound, codeTaskNotFound, "unknown task_id", r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), r)
		return
	}

	markAccessRecord(r, task.AgentID, task.ConversationID)

	payload := map[string]any{
		"task_id":         task.ID,
		"status":          task.Status,
		"conversation_id": task.ConversationID,
	}
	if task.Result != nil {
		payload["result"] = task.Result
	}
	if task.ErrorCode != "" {
		payload["error"] = map[string]string{
			"code":    task.ErrorCode,
			"message": task.ErrorMessage,
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleCancelTask requests best-effort cancellation.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing principal", r)
		return
	}

	taskID := r.PathValue("task_id")
	task, err := s.store.GetTask(r.Context(), taskID)
	if errors.Is(err, ErrNotFound) || (err == nil && task.APIKeyID != principal.APIKeyID) {
		writeError(w, http.StatusNotFound, codeTaskNotFound, "unknown task_id", r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), r)
		return
	}

	markAccessRecord(r, task.AgentID, task.ConversationID)

	s.tasks.Cancel(task.ID)
	if err := s.store.MarkTaskCanceled(r.Context(), task.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), r)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"task_id": task.ID,
		"status":  TaskCanceled,
	})
}
