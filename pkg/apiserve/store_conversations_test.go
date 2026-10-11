package apiserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func ensureConv(t *testing.T, st *Store, apiKeyID, agentID, userID, convID, sessionKey string) *Conversation {
	t.Helper()
	conv, err := st.EnsureConversation(context.Background(), EnsureConversationParams{
		APIKeyID:       apiKeyID,
		AgentID:        agentID,
		UserID:         userID,
		ConversationID: convID,
		SessionKey:     sessionKey,
	})
	require.NoError(t, err)
	return conv
}

func TestEnsureConversation_ReturnsStableRowForSameKey(t *testing.T) {
	st := newTestStore(t)

	first := ensureConv(t, st, "key_1", "main", "u_1", "conv_a", "sk_v1_aaa")
	second := ensureConv(t, st, "key_1", "main", "u_1", "conv_a", "sk_v1_aaa")

	require.Equal(t, first.SessionKey, second.SessionKey)
	require.Equal(t, "conv_a", second.ConversationID)
	require.Equal(t, "sk_v1_aaa", second.SessionKey)
	require.Equal(t, "u_1", second.UserID)
}

func TestEnsureConversation_OtherOwnerIsNotFound(t *testing.T) {
	st := newTestStore(t)

	ensureConv(t, st, "key_1", "main", "u_1", "conv_a", "sk_v1_aaa")

	_, err := st.ConversationFor(context.Background(), ConversationOwner{
		APIKeyID: "key_2", UserID: "u_1", AgentID: "main",
	}, "conv_a")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestConversationFor_UnknownReturnsErrNotFound(t *testing.T) {
	st := newTestStore(t)

	_, err := st.ConversationFor(context.Background(), ConversationOwner{
		APIKeyID: "key_1", UserID: "u_1", AgentID: "main",
	}, "conv_missing")
	require.ErrorIs(t, err, ErrNotFound)
}

// 归属校验必须在读入口内完成：同一 key 的另一个 user 读同一个 conversation_id
// 不得拿到别人的 session_key。
func TestConversationFor_OtherUserIsNotFound(t *testing.T) {
	st := newTestStore(t)

	ensureConv(t, st, "key_1", "main", "u_1", "conv_a", "sk_v1_aaa")

	_, err := st.ConversationFor(context.Background(), ConversationOwner{
		APIKeyID: "key_1", UserID: "u_2", AgentID: "main",
	}, "conv_a")
	require.ErrorIs(t, err, ErrNotFound)

	own, err := st.ConversationFor(context.Background(), ConversationOwner{
		APIKeyID: "key_1", UserID: "u_1", AgentID: "main",
	}, "conv_a")
	require.NoError(t, err)
	require.Equal(t, "sk_v1_aaa", own.SessionKey)
}

// agent 也是归属的一部分：会话键的 scope 里含 agent，若把同一 conversation_id
// 交给另一个 agent，就会用 A 的键名在 B 的 workspace 里写历史。
func TestConversationFor_OtherAgentIsNotFound(t *testing.T) {
	st := newTestStore(t)

	ensureConv(t, st, "key_1", "agent-a", "u_1", "conv_a", "sk_v1_aaa")

	_, err := st.ConversationFor(context.Background(), ConversationOwner{
		APIKeyID: "key_1", UserID: "u_1", AgentID: "agent-b",
	}, "conv_a")
	require.ErrorIs(t, err, ErrNotFound)
}
