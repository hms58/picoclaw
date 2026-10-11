package apiserve

import "github.com/sipeed/picoclaw/pkg/routing"

// Error codes returned in the canonical error envelope.
const (
	codeUnauthorized      = "unauthorized"
	codeAgentNotAllowed   = "agent_not_allowed"
	codeAgentNotFound     = "agent_not_found"
	codeSkillNotAllowed   = "skill_not_allowed"
	codeInvalidRequest    = "invalid_request"
	codeCapabilityMissing = "capability_not_supported"
	codeConversationBusy  = "conversation_busy"
	codeConversationGone  = "conversation_not_found"
	codeRateLimited       = "rate_limited"
	codeAgentTimeout      = "agent_timeout"
	codeTaskNotFound      = "task_not_found"
)

// normalizeAgentID is the single normalization helper used across the package.
func normalizeAgentID(id string) string { return routing.NormalizeAgentID(id) }

// maxRequestBodyBytes bounds request bodies to 1 MiB.
const maxRequestBodyBytes = 1 << 20

// messagesRequest is the body of POST /v1/messages and POST /v1/tasks.
type messagesRequest struct {
	AgentID        string            `json:"agent_id"`
	Message        string            `json:"message"`
	ConversationID string            `json:"conversation_id,omitempty"`
	UserID         string            `json:"user_id,omitempty"`
	Skill          string            `json:"skill,omitempty"`
	Skills         []string          `json:"skills,omitempty"`
	ToolsAllow     []string          `json:"tools_allow,omitempty"`
	TimeoutMS      int               `json:"timeout_ms,omitempty"`
	CallbackURL    string            `json:"callback_url,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// usageDTO is the usage block of a response.
type usageDTO struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// messagesResponse is the body returned by POST /v1/messages.
type messagesResponse struct {
	ConversationID string   `json:"conversation_id"`
	MessageID      string   `json:"message_id"`
	AgentID        string   `json:"agent_id"`
	Content        string   `json:"content"`
	SessionKey     string   `json:"session_key,omitempty"`
	Usage          usageDTO `json:"usage"`
	DurationMS     int64    `json:"duration_ms"`
}
