package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// patchTestProvider is a canned responder for this file only. It records what it
// was asked so a test can prove which Agent actually served the turn.
type patchTestProvider struct {
	calls        int
	lastModel    string
	lastMessages []providers.Message
}

func (p *patchTestProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	model string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	p.lastModel = model
	p.lastMessages = messages
	return &providers.LLMResponse{Content: "patch response", FinishReason: "stop"}, nil
}

// systemPrompt returns the concatenated system-role messages the provider saw.
func (p *patchTestProvider) systemPrompt() string {
	var sb strings.Builder
	for _, msg := range p.lastMessages {
		if msg.Role == "system" {
			sb.WriteString(msg.Content)
		}
	}
	return sb.String()
}

// writeSkill installs a minimal resolvable skill under a workspace.
func writeSkill(t *testing.T, workspace, name string) {
	t.Helper()
	dir := filepath.Join(workspace, "skills", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: "+name+"\ndescription: test skill "+name+"\n---\n\n# "+name+"\n"),
		0o644,
	))
}

func (p *patchTestProvider) GetDefaultModel() string { return "test-model" }

func patchTestConfig(t *testing.T, workspaceA, workspaceB string) *config.Config {
	t.Helper()
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         workspaceA,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
			List: []config.AgentConfig{
				{ID: "agent-a", Default: true, Workspace: workspaceA},
				{ID: "agent-b", Workspace: workspaceB},
			},
		},
	}
}

func TestProcessDirectWithAgent_UnknownAgentReturnsErrAgentNotFound(t *testing.T) {
	al := NewAgentLoop(patchTestConfig(t, t.TempDir(), t.TempDir()), bus.NewMessageBus(), &patchTestProvider{})

	_, err := al.ProcessDirectWithAgent(
		context.Background(), "hi", "sk_v1_probe", "api", "conv_1", "ghost-agent")

	require.ErrorIs(t, err, ErrAgentNotFound)
}

// 这条是 spec §5.6 的可测断言：请求 agent-b 时，会话文件必须落在 agent-b 的
// workspace 下，而不是默认 Agent（agent-a）的。
func TestProcessDirectWithAgent_UsesRequestedAgentWorkspace(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()

	al := NewAgentLoop(patchTestConfig(t, workspaceA, workspaceB), bus.NewMessageBus(), &patchTestProvider{})

	sessionKey := session.BuildSessionKey(session.SessionScope{
		Version:    session.ScopeVersionV1,
		AgentID:    "agent-b",
		Channel:    "api",
		Account:    "key_1",
		Dimensions: []string{"sender"},
		Values:     map[string]string{"sender": "key_1"},
	})

	response, err := al.ProcessDirectWithAgent(
		context.Background(), "hi", sessionKey, "api", "conv_1", "agent-b")

	require.NoError(t, err)
	require.Equal(t, "patch response", response)

	// sk_v1_<hex> 不含路径分隔符，文件名即 <session_key>.jsonl。
	sessionFile := sessionKey + ".jsonl"

	_, statErr := os.Stat(filepath.Join(workspaceB, "sessions", sessionFile))
	require.NoError(t, statErr, "会话文件必须落在 agent-b 的 workspace 下")

	_, statErr = os.Stat(filepath.Join(workspaceA, "sessions", sessionFile))
	require.True(t, os.IsNotExist(statErr), "默认 Agent（agent-a）的 workspace 不该被写入")
}

// 补丁的另一半：请求的 Agent 必须**驱动这一轮**，而不只是决定会话文件落在哪。
// 上面那条只证明了 workspace（副作用落点），这一条证明「哪个 Agent 的配置生效」。
//
// 判据用 system prompt，因为它活过了所有下游解析步骤，并且直接携带
// 「服务该轮的 Agent」的技能集与 workspace。
//
// 断言**刻意不用模型名**：实测（探针）两个 Agent 都会解析成 Defaults.ModelName，
// 所以模型断言在覆盖被忽略时**照样会通过**——那是空洞的。技能与 workspace 才有判别力。
func TestProcessDirectWithAgent_RouteCarriesTheRequestedAgent(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	writeSkill(t, workspaceA, "skill-a")
	writeSkill(t, workspaceB, "skill-b")

	cfg := patchTestConfig(t, workspaceA, workspaceB)
	cfg.Agents.List[0].Skills = []string{"skill-a"} // agent-a 是默认 Agent
	cfg.Agents.List[1].Skills = []string{"skill-b"}

	provider := &patchTestProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	sessionKey := session.BuildSessionKey(session.SessionScope{
		Version:    session.ScopeVersionV1,
		AgentID:    "agent-b",
		Channel:    "api",
		Account:    "key_1",
		Dimensions: []string{"sender"},
		Values:     map[string]string{"sender": "key_1"},
	})

	_, err := al.ProcessDirectWithAgent(
		context.Background(), "hi", sessionKey, "api", "conv_1", "agent-b")
	require.NoError(t, err)
	require.Equal(t, 1, provider.calls)

	prompt := provider.systemPrompt()
	require.Contains(t, prompt, "skill-b",
		"被请求 Agent 的技能集必须被加载进这一轮")
	require.NotContains(t, prompt, "skill-a",
		"覆盖若被忽略，服务这一轮的会是默认 Agent（agent-a），它的技能就会被加载")
	require.Contains(t, prompt, workspaceB)
	require.NotContains(t, prompt, workspaceA,
		"默认 Agent 的 workspace 不该出现在这一轮的提示里")
}
