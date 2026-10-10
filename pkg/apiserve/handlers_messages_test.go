package apiserve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/config"
)

func newMessageServer(t *testing.T, cfg *config.Config) (*Server, *fakeProvider, string) {
	t.Helper()
	provider := &fakeProvider{}
	if cfg == nil {
		cfg = testConfig(t)
	}
	srv, err := New(Options{Home: t.TempDir(), Config: cfg, Provider: provider})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	_, plaintext, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)
	return srv, provider, plaintext
}

func postMessages(t *testing.T, srv *Server, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodPost, "/v1/messages", body, apiKey))
	return rec
}

func TestMessages_HappyPath(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	rec := postMessages(t, srv, key, `{"agent_id":"main","message":"hello"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"content":"Mock response"`)
	require.Contains(t, rec.Body.String(), `"input_tokens":10`)
	require.Contains(t, rec.Body.String(), `"agent_id":"main"`)
	require.Contains(t, rec.Body.String(), `"conversation_id":"conv_`)
}

func TestMessages_RequiresAgentID(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	rec := postMessages(t, srv, key, `{"message":"hello"}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"invalid_request"`)
}

func TestMessages_RequiresMessage(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	rec := postMessages(t, srv, key, `{"agent_id":"main","message":"   "}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"invalid_request"`)
}

func TestMessages_UnknownAgentReturns404(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	rec := postMessages(t, srv, key, `{"agent_id":"nope","message":"hello"}`)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"agent_not_found"`)
}

func TestMessages_DisallowedAgentReturns403(t *testing.T) {
	srv, _, _ := newMessageServer(t, nil)
	_, limitedKey, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "limited", OwnerType: "internal_service",
		AllowedAgents: []string{"stock-research"},
	})
	require.NoError(t, err)

	rec := postMessages(t, srv, limitedKey, `{"agent_id":"main","message":"hello"}`)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"agent_not_allowed"`)
}

// Review Focus 4: agent ids are normalized, so case and padding must not 404.
func TestMessages_AgentIDIsNormalized(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents.List = []config.AgentConfig{
		{ID: "stock-research", Default: true, Workspace: t.TempDir()},
	}
	srv, _, key := newMessageServer(t, cfg)

	rec := postMessages(t, srv, key, `{"agent_id":"  Stock-Research  ","message":"hello"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"agent_id":"stock-research"`)
}

func TestMessages_Returns409WhenConversationBusy(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	release, err := srv.gate.TryAcquire("conv_busy")
	require.NoError(t, err)
	defer release()

	rec := postMessages(t, srv, key,
		`{"agent_id":"main","message":"hello","conversation_id":"conv_busy"}`)

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"conversation_busy"`)
}

func TestMessages_TimeoutReturns504(t *testing.T) {
	blocker := &blockingProvider{}
	blocker.block.Store(true)
	srv, err := New(Options{Home: t.TempDir(), Config: testConfig(t), Provider: blocker})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	_, key, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	rec := postMessages(t, srv, key,
		`{"agent_id":"main","message":"hello","timeout_ms":30}`)

	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"agent_timeout"`)
}

// Review Focus 1: a timed-out turn must release its conversation lock,
// otherwise the conversation is bricked at 409 forever.
func TestMessages_TimeoutReleasesConversationLock(t *testing.T) {
	blocker := &blockingProvider{}
	blocker.block.Store(true)
	srv, err := New(Options{Home: t.TempDir(), Config: testConfig(t), Provider: blocker})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	_, key, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	body := `{"agent_id":"main","message":"hello","conversation_id":"conv_lock","timeout_ms":30}`

	first := postMessages(t, srv, key, body)
	require.Equal(t, http.StatusGatewayTimeout, first.Code)

	// Same conversation must be usable again immediately.
	blocker.block.Store(false)
	second := postMessages(t, srv, key,
		`{"agent_id":"main","message":"hello","conversation_id":"conv_lock"}`)
	require.Equal(t, http.StatusOK, second.Code)
}

func TestMessages_SharedConversationKeepsHistory(t *testing.T) {
	srv, provider, key := newMessageServer(t, nil)

	first := postMessages(t, srv, key,
		`{"agent_id":"main","message":"first","conversation_id":"conv_h"}`)
	require.Equal(t, http.StatusOK, first.Code)
	firstCalls := provider.calls

	second := postMessages(t, srv, key,
		`{"agent_id":"main","message":"second","conversation_id":"conv_h"}`)
	require.Equal(t, http.StatusOK, second.Code)

	// The second turn must carry history, so the provider sees more messages
	// than a cold turn would.
	require.Greater(t, provider.calls, firstCalls)
	require.Greater(t, len(provider.lastMessages), 2)
}

func TestMessages_ResponseCarriesRecordedUsage(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	rec := postMessages(t, srv, key, `{"agent_id":"main","message":"hello"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	// fakeProvider reports 10 prompt / 5 completion tokens and the meter writes
	// them synchronously, so the response must carry the real numbers.
	require.Contains(t, rec.Body.String(), `"input_tokens":10`)
	require.Contains(t, rec.Body.String(), `"output_tokens":5`)
	require.Contains(t, rec.Body.String(), `"cost":0`)
}

func TestMessages_SkillsArrayReturns400(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	rec := postMessages(t, srv, key,
		`{"agent_id":"main","message":"hi","skills":["a","b"]}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"capability_not_supported"`)
}

func TestMessages_ToolsAllowReturns400(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	rec := postMessages(t, srv, key,
		`{"agent_id":"main","message":"hi","tools_allow":["web_search"]}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"capability_not_supported"`)
}

func TestMessages_DeniedSkillReturns403(t *testing.T) {
	srv, provider, _ := newMessageServer(t, nil)
	_, limitedKey, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "limited", OwnerType: "internal_service",
		AllowedSkills: []string{"finance-news"},
	})
	require.NoError(t, err)

	rec := postMessages(t, srv, limitedKey,
		`{"agent_id":"main","message":"帮我看看","skill":"evil-skill"}`)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"skill_not_allowed"`)
	require.Zero(t, provider.calls, "the turn must not reach the agent")
}

func TestMessages_DeniedSkillInsideMessageReturns403(t *testing.T) {
	srv, provider, _ := newMessageServer(t, nil)
	_, limitedKey, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "limited", OwnerType: "internal_service",
		AllowedSkills: []string{"finance-news"},
	})
	require.NoError(t, err)

	rec := postMessages(t, srv, limitedKey,
		`{"agent_id":"main","message":"/use evil-skill 帮我看看"}`)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"skill_not_allowed"`)
	require.Zero(t, provider.calls)
}

func TestMessages_AuthorizedSkillIsForwarded(t *testing.T) {
	srv, _, _ := newMessageServer(t, nil)
	_, key, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "limited", OwnerType: "internal_service",
		AllowedSkills: []string{"finance-news"},
	})
	require.NoError(t, err)

	rec := postMessages(t, srv, key,
		`{"agent_id":"main","message":"帮我看看","skill":"finance-news"}`)

	// 该技能名不在 loader 根内，agent 会以 "Unknown skill: ..." 正常返回 200。
	// 本用例只断言"授权通过且请求被转发"，不断言技能真的被注入。
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, rec.Body.String())
}

func TestMessages_BlocksNonUseCommands(t *testing.T) {
	srv, provider, key := newMessageServer(t, nil)

	for _, message := range []string{"/reload", "/switch gpt-4", "!stop", "/subagents"} {
		rec := postMessages(t, srv, key,
			`{"agent_id":"main","message":"`+message+`"}`)
		require.Equalf(t, http.StatusBadRequest, rec.Code, "message %q 必须被拦截", message)
		require.Contains(t, rec.Body.String(), `"code":"capability_not_supported"`)
	}
	require.Zero(t, provider.calls, "被拦截的命令不得落到 agent")
}

func TestMessages_AutoSkillIsPassedThrough(t *testing.T) {
	srv, provider, _ := newMessageServer(t, nil)
	_, key, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "limited", OwnerType: "internal_service",
		AllowedSkills: []string{"finance-news"},
	})
	require.NoError(t, err)

	rec := postMessages(t, srv, key,
		`{"agent_id":"main","message":"帮我看看","skill":"auto"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, provider.calls)
	// 消息必须原样送达，不被拼上 /use 前缀（否则会退化成"精确技能"语义）。
	last := provider.lastMessages[len(provider.lastMessages)-1]
	require.Equal(t, "帮我看看", last.Content)
}

// agent 接受 /use、!use、/USE、/use@bot 四种拼写（pkg/agent/agent_command.go:75-78），
// 接入层必须对每一种都做技能授权，否则 allowed_skills 可被绕过。
func TestMessages_DeniedSkillAnySpellingReturns403(t *testing.T) {
	srv, provider, _ := newMessageServer(t, nil)
	_, limitedKey, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "limited", OwnerType: "internal_service",
		AllowedSkills: []string{"finance-news"},
	})
	require.NoError(t, err)

	for _, message := range []string{
		"/use evil-skill 帮我看看",
		"!use evil-skill 帮我看看",
		"/USE evil-skill 帮我看看",
		"/use@bot evil-skill 帮我看看",
	} {
		rec := postMessages(t, srv, limitedKey,
			`{"agent_id":"main","message":"`+message+`"}`)
		require.Equalf(t, http.StatusForbidden, rec.Code, "message %q 必须被拒", message)
		require.Contains(t, rec.Body.String(), `"code":"skill_not_allowed"`)
	}
	require.Zero(t, provider.calls, "被拒的技能不得落到 agent")
}

// 同一 conversation_id 换 agent 必须被拒：会话键的 scope 里含 agent，
// 放行就会让 agent-b 用 agent-a 的键名执行（spec 事实 13 的镜像场景）。
func TestMessages_OtherAgentOnExistingConversationIsRejected(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents.List = []config.AgentConfig{
		{ID: "agent-a", Default: true, Workspace: t.TempDir()},
		{ID: "agent-b", Workspace: t.TempDir()},
	}
	srv, _, key := newMessageServer(t, cfg)

	first := postMessages(t, srv, key,
		`{"agent_id":"agent-a","message":"hello","conversation_id":"conv_ab"}`)
	require.Equal(t, http.StatusOK, first.Code)

	second := postMessages(t, srv, key,
		`{"agent_id":"agent-b","message":"hello","conversation_id":"conv_ab"}`)
	require.Equal(t, http.StatusNotFound, second.Code)
	require.Contains(t, second.Body.String(), `"code":"conversation_not_found"`)
}

// 本计划最关键的一条测试：agent_id 必须真的决定"由哪个 Agent 处理"。
// 没有 Task 12 的补丁时它会失败 —— 请求跑在默认 Agent 上，会话文件落进 agent-a 的 workspace，
// 而响应体与审计里仍然写着 agent_id=agent-b（spec 事实 13 描述的"归属与能力错配且不报错"）。
func TestMessages_RoutesToRequestedAgent(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()

	cfg := testConfig(t)
	cfg.Agents.List = []config.AgentConfig{
		{ID: "agent-a", Default: true, Workspace: workspaceA},
		{ID: "agent-b", Workspace: workspaceB},
	}
	srv, _, key := newMessageServer(t, cfg)

	rec := postMessages(t, srv, key, `{"agent_id":"agent-b","message":"hello"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"agent_id":"agent-b"`)

	var payload struct {
		SessionKey string `json:"session_key"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.NotEmpty(t, payload.SessionKey)

	// sk_v1_<hex> 不含路径分隔符，文件名就是 <session_key>.jsonl（pkg/memory/jsonl.go:43-52）。
	sessionFile := payload.SessionKey + ".jsonl"

	_, statErr := os.Stat(filepath.Join(workspaceB, "sessions", sessionFile))
	require.NoError(t, statErr, "会话文件必须落在被请求 Agent（agent-b）的 workspace 下")

	_, statErr = os.Stat(filepath.Join(workspaceA, "sessions", sessionFile))
	require.True(t, os.IsNotExist(statErr), "默认 Agent（agent-a）的 workspace 不该有这个会话文件")
}
