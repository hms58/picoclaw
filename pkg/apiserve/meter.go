package apiserve

import (
	"context"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// MeterContext carries the attribution dimensions of one API-originated turn.
type MeterContext struct {
	APIKeyID       string
	AgentID        string
	UserID         string
	ConversationID string
	SessionKey     string
	RequestID      string
	Model          string
}

type meterCtxKey struct{}

// WithMeterContext attaches the attribution dimensions to the context.
func WithMeterContext(ctx context.Context, mc MeterContext) context.Context {
	return context.WithValue(ctx, meterCtxKey{}, mc)
}

// MeterContextFrom extracts the attribution dimensions.
func MeterContextFrom(ctx context.Context) (MeterContext, bool) {
	mc, ok := ctx.Value(meterCtxKey{}).(MeterContext)
	return mc, ok
}

// UsageRecord is one recorded LLM call.
type UsageRecord struct {
	APIKeyID         string
	AgentID          string
	UserID           string
	ConversationID   string
	SessionKey       string
	RequestID        string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CreatedAt        time.Time
}

// UsageRecorder persists usage records. *Store implements it.
type UsageRecorder interface {
	RecordUsage(ctx context.Context, rec UsageRecord) error
}

// MeteredProvider wraps an LLMProvider and records the usage of every call that
// carries a MeterContext. Calls without one (heartbeats, cron) are ignored.
type MeteredProvider struct {
	inner providers.LLMProvider
	rec   UsageRecorder
}

// NewMeteredProvider wraps inner. A nil rec disables recording.
func NewMeteredProvider(inner providers.LLMProvider, rec UsageRecorder) *MeteredProvider {
	return &MeteredProvider{inner: inner, rec: rec}
}

// Chat forwards to the wrapped provider and records the resulting usage.
func (m *MeteredProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	resp, err := m.inner.Chat(ctx, messages, tools, model, options)
	if err != nil {
		return nil, err
	}
	m.record(ctx, model, resp)
	return resp, nil
}

// GetDefaultModel forwards to the wrapped provider.
func (m *MeteredProvider) GetDefaultModel() string { return m.inner.GetDefaultModel() }

// ChatStream forwards a streaming call and records usage.
//
// Re-exposing the optional provider interfaces is mandatory, not cosmetic: the
// agent type-asserts them on the live provider (pkg/agent/thinking.go:98,
// pkg/agent/prompt_turn.go:70, pkg/agent/pipeline_streaming.go:29,80,
// pkg/agent/instance.go:709). A wrapper that hides them silently disables
// thinking_level, native search, streaming and provider Close(). This mirrors the
// existing in-repo wrapper (pkg/providers/tool_schema_transform.go:78-107).
func (m *MeteredProvider) ChatStream(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(accumulated string),
) (*providers.LLMResponse, error) {
	streaming, ok := m.inner.(providers.StreamingProvider)
	if !ok {
		// Degrade to a non-streaming call so the capability query and the call
		// agree. Nothing in this service configures a bus stream delegate today,
		// so the agent's streaming path is unreachable either way.
		return m.Chat(ctx, messages, tools, model, options)
	}

	resp, err := streaming.ChatStream(ctx, messages, tools, model, options, onChunk)
	if err != nil {
		return nil, err
	}
	m.record(ctx, model, resp)
	return resp, nil
}

// ChatStreamEvents forwards an event-streaming call and records usage.
func (m *MeteredProvider) ChatStreamEvents(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(providers.StreamChunk),
) (*providers.LLMResponse, error) {
	events, ok := m.inner.(providers.StreamingEventProvider)
	if !ok {
		return m.ChatStream(ctx, messages, tools, model, options, func(accumulated string) {
			if onChunk != nil {
				onChunk(providers.StreamChunk{Content: accumulated})
			}
		})
	}

	resp, err := events.ChatStreamEvents(ctx, messages, tools, model, options, onChunk)
	if err != nil {
		return nil, err
	}
	m.record(ctx, model, resp)
	return resp, nil
}

// SupportsThinking reports whether the wrapped provider supports extended
// thinking. Returning false for a non-capable inner keeps the agent's
// "not supported" path identical to the unwrapped provider.
func (m *MeteredProvider) SupportsThinking() bool {
	tc, ok := m.inner.(providers.ThinkingCapable)
	return ok && tc.SupportsThinking()
}

// SupportsNativeSearch reports whether the wrapped provider has built-in search.
func (m *MeteredProvider) SupportsNativeSearch() bool {
	ns, ok := m.inner.(providers.NativeSearchCapable)
	return ok && ns.SupportsNativeSearch()
}

// Close releases the wrapped provider when it is stateful, so connection pools
// are not leaked on shutdown or reload.
func (m *MeteredProvider) Close() {
	if stateful, ok := m.inner.(providers.StatefulProvider); ok {
		stateful.Close()
	}
}

func (m *MeteredProvider) record(ctx context.Context, model string, resp *providers.LLMResponse) {
	if m.rec == nil || resp == nil {
		return
	}
	mc, ok := MeterContextFrom(ctx)
	if !ok {
		return
	}

	rec := UsageRecord{
		APIKeyID:       mc.APIKeyID,
		AgentID:        mc.AgentID,
		UserID:         mc.UserID,
		ConversationID: mc.ConversationID,
		SessionKey:     mc.SessionKey,
		RequestID:      mc.RequestID,
		Model:          mc.Model,
		CreatedAt:      time.Now(),
	}
	if rec.Model == "" {
		rec.Model = model
	}
	if resp.Usage != nil {
		rec.PromptTokens = resp.Usage.PromptTokens
		rec.CompletionTokens = resp.Usage.CompletionTokens
		rec.TotalTokens = resp.Usage.TotalTokens
	}

	// Recording must survive request cancellation, otherwise every timeout
	// loses its audit row. Never fail the turn because of a metering error — but
	// never swallow it silently either, or a broken audit trail is unobservable.
	if err := m.rec.RecordUsage(context.WithoutCancel(ctx), rec); err != nil {
		logger.WarnCF("apiserve", "failed to record usage", map[string]any{
			"api_key_id": mc.APIKeyID,
			"agent_id":   mc.AgentID,
			"request_id": mc.RequestID,
			"error":      err.Error(),
		})
	}
}
