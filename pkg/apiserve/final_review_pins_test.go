package apiserve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The whole-branch final review named a set of gaps to pin before merging. These
// are those pins: each closes a hole where behaviour was correct but untested, so
// a regression would have shipped silently.

// Every /v1 route must reject an anonymous request. Testing only /v1/agents
// leaves every other route's withMiddleware wrapper unprotected by a test —
// dropping the wrapper from one route would be invisible.
func TestAuth_EveryV1RouteRejectsAnAnonymousRequest(t *testing.T) {
	srv := newTestServer(t)

	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/agents"},
		{http.MethodPost, "/v1/messages"},
		{http.MethodPost, "/v1/tasks"},
		{http.MethodGet, "/v1/tasks/task_probe"},
		{http.MethodPost, "/v1/tasks/task_probe/cancel"},
		{http.MethodGet, "/v1/usage"},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(route.method, route.path, nil))

			require.Equal(t, http.StatusUnauthorized, rec.Code,
				"route %s %s must not answer without a bearer token", route.method, route.path)
			require.Contains(t, rec.Body.String(), `"code":"unauthorized"`)
		})
	}
}

// The 1 MiB cap is a real DoS boundary and was untested.
func TestMessages_RejectsAnOversizedBodyWithoutReachingTheAgent(t *testing.T) {
	srv, provider, key := newMessageServer(t, nil)

	blob := strings.Repeat("a", maxRequestBodyBytes+4096)
	body := `{"agent_id":"main","message":"` + blob + `"}`

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodPost, "/v1/messages", body, key))

	// 413 specifically: MaxBytesReader writes that status itself, so answering
	// 400 would be a second, contradictory WriteHeader.
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Zero(t, provider.calls, "an oversized body must not reach the agent")
	require.Equal(t, 1<<20, maxRequestBodyBytes, "the documented cap is 1 MiB")
}

// Every queryable usage column must round-trip. A slipped parameter in the
// INSERT would otherwise pass unnoticed because the existing test uses one row.
func TestRecordUsage_PinsEveryQueryableColumn(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, st.RecordUsage(ctx, UsageRecord{
		APIKeyID: "key_1", AgentID: "agent-x", UserID: "user-x",
		ConversationID: "conv-x", SessionKey: "sk_v1_x", RequestID: "req-x", Model: "model-x",
		PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33, CreatedAt: now,
	}))
	require.NoError(t, st.RecordUsage(ctx, UsageRecord{
		APIKeyID: "key_2", AgentID: "agent-y", UserID: "user-y",
		PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, CreatedAt: now,
	}))

	byAgent, err := st.SumUsage(ctx, UsageQuery{AgentID: "agent-x"})
	require.NoError(t, err)
	require.Len(t, byAgent, 1)
	require.Equal(t, 1, byAgent[0].Requests)
	require.Equal(t, 11, byAgent[0].PromptTokens)
	require.Equal(t, 22, byAgent[0].CompletionTokens)
	require.Equal(t, 33, byAgent[0].TotalTokens)

	byKey, err := st.SumUsage(ctx, UsageQuery{APIKeyID: "key_2"})
	require.NoError(t, err)
	require.Len(t, byKey, 1)
	require.Equal(t, 3, byKey[0].TotalTokens)

	byUser, err := st.SumUsage(ctx, UsageQuery{GroupBy: "user"})
	require.NoError(t, err)
	require.Len(t, byUser, 2, "user_id must actually be written to its own column")
	buckets := map[string]int{}
	for _, row := range byUser {
		buckets[row.Key] = row.TotalTokens
	}
	require.Equal(t, 33, buckets["user-x"])
	require.Equal(t, 3, buckets["user-y"])

	byRequest, err := st.SumUsageByRequest(ctx, "key_1", "req-x")
	require.NoError(t, err)
	require.Equal(t, 11, byRequest.PromptTokens, "request_id must round-trip")
	require.Equal(t, 22, byRequest.CompletionTokens)

	// The same request id under a different key selects nothing.
	other, err := st.SumUsageByRequest(ctx, "key_2", "req-x")
	require.NoError(t, err)
	require.Zero(t, other.Requests)
}

// The handler converts `skill` into the "/use <name> " prefix, and that is the
// only hard skill boundary the API owns. Before this test the conversion was
// asserted but the *effect* was not: nothing proved a resolved skill actually
// reaches the agent. An unresolvable skill makes the agent short-circuit, so the
// provider call count is the discriminator.
func TestSkill_AResolvableSkillReachesTheAgentAnUnresolvableOneDoesNot(t *testing.T) {
	cfg := testConfig(t)
	skillDir := filepath.Join(cfg.Agents.Defaults.Workspace, "skills", "finance-news")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: finance-news\ndescription: test skill\n---\n\n# finance-news\n"),
		0o644,
	))

	srv, provider, key := newMessageServer(t, cfg)

	known := postMessages(t, srv, key, `{"agent_id":"main","message":"帮我看看","skill":"finance-news"}`)
	require.Equal(t, http.StatusOK, known.Code)
	require.Equal(t, 1, provider.calls,
		"a resolvable skill must be armed and reach the LLM")

	unknown := postMessages(t, srv, key, `{"agent_id":"main","message":"帮我看看","skill":"no-such-skill"}`)
	require.Equal(t, http.StatusOK, unknown.Code)
	require.Equal(t, 1, provider.calls,
		"an unresolvable skill short-circuits inside the agent, so it adds no LLM call")
}

// fakeAddr is the minimum net.Addr a fake listener needs.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// failingListener makes http.Server.Serve return a permanent error immediately,
// so Start reaches its serveErr branch having already launched the task pool.
type failingListener struct{}

func (l *failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }
func (l *failingListener) Close() error              { return nil }
func (l *failingListener) Addr() net.Addr            { return fakeAddr("127.0.0.1:0") }

// A failed Serve must shut the server down. Without it the task pool, the
// AgentLoop and the store all leak for the life of the process, and the caller
// only sees an error return. The observable is that Shutdown ran, which closes
// the store.
func TestStart_ShutsDownOnServeFailureInsteadOfLeakingThePool(t *testing.T) {
	srv, err := New(Options{
		Home:     t.TempDir(),
		Config:   testConfig(t),
		Provider: &fakeProvider{},
		Listener: &failingListener{},
	})
	require.NoError(t, err)
	// No t.Cleanup Shutdown here: the assertion below is that Start did it.

	done := make(chan error, 1)
	go func() { done <- srv.Start(context.Background()) }()

	select {
	case startErr := <-done:
		require.Error(t, startErr, "a failing listener must surface as an error")
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the listener failed")
	}

	usageErr := srv.store.RecordUsage(context.Background(), UsageRecord{
		APIKeyID: "key_probe", AgentID: "main",
	})
	require.Error(t, usageErr,
		"Start must have shut down (closing the store) instead of returning early and leaking it")
}

// The callback body hardcodes status "succeeded". Without a guard, a task the
// caller cancelled while its turn still completed gets reported as a success.
func TestDeliverCallback_RefusesToReportSuccessForACanceledTask(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	hits := make(chan struct{}, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)

	// A real key: without it the signing-key lookup fails and the callback would
	// be skipped for the wrong reason, hiding the defect.
	created, _, err := srv.store.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "hook-target", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	task, err := srv.store.CreateTask(ctx, CreateTaskParams{
		APIKeyID:       created.ID,
		AgentID:        "main",
		ConversationID: "conv_cancel",
		SessionKey:     "sk_v1_cancel",
		CallbackURL:    hook.URL,
		Request:        messagesRequest{AgentID: "main", Message: "hi"},
	})
	require.NoError(t, err)
	require.NoError(t, srv.store.MarkTaskCanceled(ctx, task.ID))

	reloaded, err := srv.store.GetTask(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, TaskCanceled, reloaded.Status)

	NewTaskRunner(srv).deliverCallback(ctx, reloaded, &messagesResponse{Content: "done"})

	select {
	case <-hits:
		t.Fatal("a cancelled task must not be announced as succeeded")
	case <-time.After(300 * time.Millisecond):
	}
}
