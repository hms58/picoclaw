package apiserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Review Focus 3: two conversations of the same API key must not share history.
func TestEnsureConversation_DifferentConversationsGetDifferentSessionKeys(t *testing.T) {
	srv := newTestServer(t)
	p := &Principal{APIKeyID: "key_1", Name: "svc"}

	first, err := srv.ensureConversation(context.Background(), p, "main", "u_1", "conv_a")
	require.NoError(t, err)
	second, err := srv.ensureConversation(context.Background(), p, "main", "u_1", "conv_b")
	require.NoError(t, err)

	require.NotEqual(t, first.SessionKey, second.SessionKey)
}

// Review Focus 2: an empty user_id must fall back to the API key identity so
// the sender dimension is never dropped and sessions do not merge.
func TestIdentity_EmptyUserIDFallsBackToAPIKey(t *testing.T) {
	p := &Principal{APIKeyID: "key_1"}

	account, sender := identityFor(p, "")
	require.Equal(t, "key_1", account)
	require.Equal(t, "key_1", sender)

	other := &Principal{APIKeyID: "key_2"}
	otherAccount, otherSender := identityFor(other, "")
	require.NotEqual(t, account, otherAccount)
	require.NotEqual(t, sender, otherSender)
}

func TestIdentity_TenantIDTakesPrecedenceForAccount(t *testing.T) {
	p := &Principal{APIKeyID: "key_1", TenantID: "tenant_9"}

	account, sender := identityFor(p, "u_1")
	require.Equal(t, "tenant_9", account)
	require.Equal(t, "u_1", sender)
}

func TestEnsureConversation_SameUserSameConversationIsStable(t *testing.T) {
	srv := newTestServer(t)
	p := &Principal{APIKeyID: "key_1"}

	first, err := srv.ensureConversation(context.Background(), p, "main", "u_1", "conv_a")
	require.NoError(t, err)
	second, err := srv.ensureConversation(context.Background(), p, "main", "u_1", "conv_a")
	require.NoError(t, err)

	require.Equal(t, first.SessionKey, second.SessionKey)
}

// 会话归属于一个 (api key, user) 对：另一个 user 拿同一个 conversation_id
// 必须被拒绝，否则会继承原属主的会话历史。
func TestEnsureConversation_OtherUserIsNotFound(t *testing.T) {
	srv := newTestServer(t)
	p := &Principal{APIKeyID: "key_1"}

	first, err := srv.ensureConversation(context.Background(), p, "main", "u_1", "conv_a")
	require.NoError(t, err)
	require.Equal(t, "u_1", first.UserID)

	_, err = srv.ensureConversation(context.Background(), p, "main", "u_2", "conv_a")
	require.ErrorIs(t, err, ErrNotFound)
}

// caller 传入的大小写/空白变体必须落到同一个 conversation 与同一个 session_key。
// 否则第二次插入会撞 session_key 主键（ON CONFLICT(conversation_id) 覆盖不到）→ 500。
func TestEnsureConversation_NormalizesConversationID(t *testing.T) {
	srv := newTestServer(t)
	p := &Principal{APIKeyID: "key_1"}

	first, err := srv.ensureConversation(context.Background(), p, "main", "u_1", "conv_a")
	require.NoError(t, err)

	second, err := srv.ensureConversation(context.Background(), p, "main", "u_1", "  CONV_A  ")
	require.NoError(t, err, "大小写/空白变体不该报错")

	require.Equal(t, first.ConversationID, second.ConversationID)
	require.Equal(t, first.SessionKey, second.SessionKey)
}

func TestEnsureConversation_GeneratesConversationIDWhenEmpty(t *testing.T) {
	srv := newTestServer(t)
	p := &Principal{APIKeyID: "key_1"}

	conv, err := srv.ensureConversation(context.Background(), p, "main", "u_1", "")
	require.NoError(t, err)
	require.NotEmpty(t, conv.ConversationID)
	require.Contains(t, conv.ConversationID, "conv_")
}

func TestGate_BlocksConcurrentAndReleasesOnDemand(t *testing.T) {
	gate := NewGate()

	release, err := gate.TryAcquire("conv_a")
	require.NoError(t, err)

	_, err = gate.TryAcquire("conv_a")
	require.ErrorIs(t, err, ErrConversationBusy)

	// A different conversation is unaffected.
	otherRelease, err := gate.TryAcquire("conv_b")
	require.NoError(t, err)
	otherRelease()

	release()

	secondRelease, err := gate.TryAcquire("conv_a")
	require.NoError(t, err)
	secondRelease()
}

func TestGate_ReleaseIsIdempotent(t *testing.T) {
	gate := NewGate()

	release, err := gate.TryAcquire("conv_a")
	require.NoError(t, err)

	release()
	release() // must not panic or release someone else's lock

	release2, err := gate.TryAcquire("conv_a")
	require.NoError(t, err)
	release2()
}
