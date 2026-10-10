package apiserve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// fakeProvider returns a canned reply and records what it was asked.
// It implements providers.LLMProvider.
type fakeProvider struct {
	reply        string
	calls        int
	lastMessages []providers.Message
	lastModel    string
}

func (p *fakeProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	model string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	p.lastMessages = messages
	p.lastModel = model
	reply := p.reply
	if reply == "" {
		reply = "Mock response"
	}
	return &providers.LLMResponse{
		Content:      reply,
		FinishReason: "stop",
		Usage: &providers.UsageInfo{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}, nil
}

func (p *fakeProvider) GetDefaultModel() string { return "test-model" }

// testConfig mirrors the minimal config shape used by pkg/agent tests.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := New(Options{
		Home:     t.TempDir(),
		Config:   testConfig(t),
		Provider: &fakeProvider{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

// blockingProvider blocks in Chat until the request context ends whenever
// block is set. Used to exercise timeouts and lock release.
type blockingProvider struct {
	fakeProvider
	block atomic.Bool
}

func (p *blockingProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	if p.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.fakeProvider.Chat(ctx, messages, tools, model, options)
}

// taskIDFrom reads the task_id out of a POST /v1/tasks response body.
func taskIDFrom(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode task response: %v", err)
	}
	if payload.TaskID == "" {
		t.Fatalf("task_id missing in %s", string(body))
	}
	return payload.TaskID
}

// newAuthedRequest builds a request carrying a bearer token.
func newAuthedRequest(t *testing.T, method, path, body, apiKey string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}
