package apiserve

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type recordingRecorder struct {
	mu      sync.Mutex
	records []UsageRecord
	ctxErrs []error
}

// RecordUsage also captures ctx.Err() as handed to the recorder, so tests can
// prove the context was de-cancelled before recording.
func (r *recordingRecorder) RecordUsage(ctx context.Context, rec UsageRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	return nil
}

func (r *recordingRecorder) all() []UsageRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]UsageRecord(nil), r.records...)
}

func (r *recordingRecorder) recordedCtxErrs() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.ctxErrs...)
}

func TestMeteredProvider_RecordsUsageWithContext(t *testing.T) {
	rec := &recordingRecorder{}
	metered := NewMeteredProvider(&fakeProvider{}, rec)

	ctx := WithMeterContext(context.Background(), MeterContext{
		APIKeyID:       "key_1",
		AgentID:        "main",
		UserID:         "u_1",
		ConversationID: "conv_a",
		SessionKey:     "sk_v1_aaa",
		RequestID:      "req_1",
	})

	_, err := metered.Chat(ctx, nil, nil, "test-model", nil)
	require.NoError(t, err)

	records := rec.all()
	require.Len(t, records, 1)
	got := records[0]
	require.Equal(t, "key_1", got.APIKeyID)
	require.Equal(t, "main", got.AgentID)
	require.Equal(t, "u_1", got.UserID)
	require.Equal(t, "conv_a", got.ConversationID)
	require.Equal(t, "sk_v1_aaa", got.SessionKey)
	require.Equal(t, "req_1", got.RequestID)
	require.Equal(t, "test-model", got.Model)
	require.Equal(t, 10, got.PromptTokens)
	require.Equal(t, 5, got.CompletionTokens)
	require.Equal(t, 15, got.TotalTokens)
}

func TestMeteredProvider_WithoutMeterContextRecordsNothing(t *testing.T) {
	rec := &recordingRecorder{}
	metered := NewMeteredProvider(&fakeProvider{}, rec)

	_, err := metered.Chat(context.Background(), nil, nil, "test-model", nil)
	require.NoError(t, err)
	require.Empty(t, rec.all())
}

func TestMeteredProvider_ProviderErrorRecordsNothing(t *testing.T) {
	rec := &recordingRecorder{}
	metered := NewMeteredProvider(&errorProvider{}, rec)

	ctx := WithMeterContext(context.Background(), MeterContext{APIKeyID: "key_1"})
	_, err := metered.Chat(ctx, nil, nil, "test-model", nil)
	require.Error(t, err)
	require.Empty(t, rec.all())
}

func TestMeteredProvider_PassesThroughDefaultModel(t *testing.T) {
	metered := NewMeteredProvider(&fakeProvider{}, &recordingRecorder{})
	require.Equal(t, "test-model", metered.GetDefaultModel())
}

// Usage must still be recorded when the request context is already cancelled,
// otherwise every timeout silently loses its audit record.
func TestMeteredProvider_RecordsAfterContextCancelled(t *testing.T) {
	rec := &recordingRecorder{}
	metered := NewMeteredProvider(&fakeProvider{}, rec)

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithMeterContext(ctx, MeterContext{APIKeyID: "key_1"})
	cancel()

	_, err := metered.Chat(ctx, nil, nil, "test-model", nil)
	require.NoError(t, err)
	require.Len(t, rec.all(), 1)

	// Discriminating: without context.WithoutCancel the recorder receives a
	// cancelled context and this is context.Canceled, not nil.
	require.Equal(t, []error{nil}, rec.recordedCtxErrs())
}

// capableProvider implements the optional ThinkingCapable, NativeSearchCapable
// and StatefulProvider interfaces.
type capableProvider struct {
	fakeProvider
	closed int
}

func (p *capableProvider) SupportsThinking() bool     { return true }
func (p *capableProvider) SupportsNativeSearch() bool { return true }
func (p *capableProvider) Close()                     { p.closed++ }

// streamingProvider implements StreamingProvider.
type streamingProvider struct {
	fakeProvider
	streamCalls int
}

func (p *streamingProvider) ChatStream(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(accumulated string),
) (*providers.LLMResponse, error) {
	p.streamCalls++
	if onChunk != nil {
		onChunk("streamed ")
	}
	return p.fakeProvider.Chat(ctx, messages, tools, model, options)
}

// Wrapping must not hide the optional provider interfaces the agent asserts on,
// otherwise thinking_level / native search / Close() are silently disabled.
func TestMeteredProvider_DelegatesOptionalCapabilities(t *testing.T) {
	capable := NewMeteredProvider(&capableProvider{}, &recordingRecorder{})
	require.True(t, capable.SupportsThinking())
	require.True(t, capable.SupportsNativeSearch())

	var _ providers.ThinkingCapable = capable
	var _ providers.NativeSearchCapable = capable
	var _ providers.StatefulProvider = capable

	plain := NewMeteredProvider(&fakeProvider{}, &recordingRecorder{})
	require.False(t, plain.SupportsThinking(), "非能力型 provider 不得被报告为支持")
	require.False(t, plain.SupportsNativeSearch())
}

func TestMeteredProvider_ClosesStatefulInner(t *testing.T) {
	inner := &capableProvider{}
	metered := NewMeteredProvider(inner, &recordingRecorder{})

	metered.Close()
	require.Equal(t, 1, inner.closed)

	// A non-stateful inner must not panic.
	NewMeteredProvider(&fakeProvider{}, &recordingRecorder{}).Close()
}

func TestMeteredProvider_ChatStreamDelegatesAndRecords(t *testing.T) {
	rec := &recordingRecorder{}
	inner := &streamingProvider{}
	metered := NewMeteredProvider(inner, rec)
	ctx := WithMeterContext(context.Background(), MeterContext{APIKeyID: "key_1"})

	var chunks []string
	resp, err := metered.ChatStream(ctx, nil, nil, "test-model", nil, func(accumulated string) {
		chunks = append(chunks, accumulated)
	})

	require.NoError(t, err)
	require.Equal(t, "Mock response", resp.Content)
	require.Equal(t, 1, inner.streamCalls)
	require.Equal(t, []string{"streamed "}, chunks)
	require.Len(t, rec.all(), 1)
}

func TestMeteredProvider_ChatStreamFallsBackToChat(t *testing.T) {
	rec := &recordingRecorder{}
	metered := NewMeteredProvider(&fakeProvider{}, rec)
	ctx := WithMeterContext(context.Background(), MeterContext{APIKeyID: "key_1"})

	resp, err := metered.ChatStream(ctx, nil, nil, "test-model", nil, nil)

	require.NoError(t, err)
	require.Equal(t, "Mock response", resp.Content)
	require.Len(t, rec.all(), 1)
}

func TestMeteredProvider_ChatStreamEventsFallsBackToChat(t *testing.T) {
	rec := &recordingRecorder{}
	metered := NewMeteredProvider(&fakeProvider{}, rec)
	ctx := WithMeterContext(context.Background(), MeterContext{APIKeyID: "key_1"})

	var chunks []providers.StreamChunk
	resp, err := metered.ChatStreamEvents(ctx, nil, nil, "test-model", nil,
		func(c providers.StreamChunk) { chunks = append(chunks, c) })

	require.NoError(t, err)
	require.Equal(t, "Mock response", resp.Content)
	require.Empty(t, chunks, "非流式 inner 不会产生增量块")
	require.Len(t, rec.all(), 1)
}

// errorProvider always fails.
type errorProvider struct{}

func (p *errorProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	return nil, errors.New("boom")
}

func (p *errorProvider) GetDefaultModel() string { return "test-model" }
