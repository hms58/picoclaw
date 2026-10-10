package apiserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// resolveTimeout clamps the requested timeout into the configured window.
func (s *Server) resolveTimeout(ms int) time.Duration {
	if ms <= 0 {
		return s.settings.DefaultTimeout
	}
	d := time.Duration(ms) * time.Millisecond
	if d > s.settings.MaxTimeout {
		return s.settings.MaxTimeout
	}
	return d
}

// handleMessages runs one synchronous agent turn.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing principal", r)
		return
	}

	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if s.replayIdempotent(w, r, idemKey, http.StatusOK) {
		return
	}

	var req messagesRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err := decoder.Decode(&req); err != nil {
		// MaxBytesReader has already written 413 itself, so answering 400 here
		// would be a second WriteHeader (a no-op plus a superfluous-header log).
		// Report the same status it did.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, codeInvalidRequest,
				"request body exceeds 1 MiB", r)
			return
		}
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid json body", r)
		return
	}

	// capabilities that require the P1 patch must fail loudly, not silently
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
		// Either the conversation_id is unknown, or it belongs to a different
		// (api key, user, agent). Both are indistinguishable to the caller on
		// purpose — same rule as an unknown API key.
		writeError(w, http.StatusNotFound, codeConversationGone,
			"unknown conversation_id", r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), r)
		return
	}

	release, err := s.gate.TryAcquire(conv.ConversationID)
	if errors.Is(err, ErrConversationBusy) {
		writeError(w, http.StatusConflict, codeConversationBusy,
			"conversation already has an active turn", r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), r)
		return
	}
	defer release()

	// Enrich the access record now that the agent and conversation are known.
	markAccessRecord(r, agentID, conv.ConversationID)

	requestID := requestIDFrom(r)
	ctx, cancel := context.WithTimeout(r.Context(), s.resolveTimeout(req.TimeoutMS))
	defer cancel()
	ctx = WithMeterContext(ctx, MeterContext{
		APIKeyID:       principal.APIKeyID,
		AgentID:        agentID,
		UserID:         conv.UserID,
		ConversationID: conv.ConversationID,
		SessionKey:     conv.SessionKey,
		RequestID:      requestID,
	})

	started := time.Now()
	// Task 12 的补丁入口：显式指定 Agent。
	// 绝不要换成 ProcessDirectWithChannel —— 它会把 agentID 静默忽略（spec 事实 13），
	// 请求会跑在默认 Agent 上而契约字段还写着目标 agent_id。
	content, err := s.loop.ProcessDirectWithAgent(
		ctx, message, conv.SessionKey, "api", conv.ConversationID, agentID,
	)
	if err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, codeAgentTimeout,
				"agent turn exceeded timeout_ms", r)
		case errors.Is(err, agent.ErrAgentNotFound):
			// 防御性分支：上面的 resolveAgent 已校验过，正常走不到这里。
			writeError(w, http.StatusNotFound, codeAgentNotFound, "unknown agent_id: "+agentID, r)
		default:
			logger.ErrorCF("apiserve", "agent turn failed", map[string]any{
				"request_id":      requestID,
				"conversation_id": conv.ConversationID,
				"agent_id":        agentID,
				"error":           err.Error(),
			})
			writeError(w, http.StatusInternalServerError, "agent_error", err.Error(), r)
		}
		return
	}

	usage := usageDTO{}
	if totals, totalsErr := s.store.SumUsageByRequest(
		context.WithoutCancel(r.Context()), principal.APIKeyID, requestID,
	); totalsErr == nil {
		usage.InputTokens = totals.PromptTokens
		usage.OutputTokens = totals.CompletionTokens
	}
	markAccessUsage(r, usage.InputTokens, usage.OutputTokens)

	response := messagesResponse{
		ConversationID: conv.ConversationID,
		MessageID:      fmt.Sprintf("msg_%s", requestID),
		AgentID:        agentID,
		Content:        content,
		SessionKey:     conv.SessionKey,
		Usage:          usage,
		DurationMS:     time.Since(started).Milliseconds(),
	}

	s.storeIdempotent(r.Context(), idemKey, response)

	writeJSON(w, http.StatusOK, response)
}
