package apiserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsage_GroupsByAgent(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	_, key, err := srv.store.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	principal, err := srv.store.LookupAPIKey(ctx, key)
	require.NoError(t, err)

	now := time.Now()
	require.NoError(t, srv.store.RecordUsage(ctx, UsageRecord{
		APIKeyID: principal.ID, AgentID: "main", TotalTokens: 15,
		PromptTokens: 10, CompletionTokens: 5, CreatedAt: now,
	}))
	require.NoError(t, srv.store.RecordUsage(ctx, UsageRecord{
		APIKeyID: principal.ID, AgentID: "main", TotalTokens: 25,
		PromptTokens: 20, CompletionTokens: 5, CreatedAt: now,
	}))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodGet,
		"/v1/usage?group_by=agent", "", key))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"key":"main"`)
	require.Contains(t, rec.Body.String(), `"requests":2`)
	require.Contains(t, rec.Body.String(), `"total_tokens":40`)
}

// 跨 key 隔离：一个 key 的用量绝不能出现在另一个 key 的响应里。
// 这条是安全属性，必须真的被钉住（只有自己数据的池子无法判别过滤是否生效）。
func TestUsage_NeverLeaksAnotherKeysUsage(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	_, mine, err := srv.store.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "mine", OwnerType: "internal_service",
	})
	require.NoError(t, err)
	_, theirs, err := srv.store.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "theirs", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	myPrincipal, err := srv.store.LookupAPIKey(ctx, mine)
	require.NoError(t, err)
	theirPrincipal, err := srv.store.LookupAPIKey(ctx, theirs)
	require.NoError(t, err)

	now := time.Now()
	require.NoError(t, srv.store.RecordUsage(ctx, UsageRecord{
		APIKeyID: myPrincipal.ID, AgentID: "main", TotalTokens: 15,
		PromptTokens: 10, CompletionTokens: 5, CreatedAt: now,
	}))
	require.NoError(t, srv.store.RecordUsage(ctx, UsageRecord{
		APIKeyID: theirPrincipal.ID, AgentID: "main", TotalTokens: 999,
		PromptTokens: 999, CompletionTokens: 0, CreatedAt: now,
	}))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodGet,
		"/v1/usage?group_by=agent", "", mine))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"requests":1`)
	require.Contains(t, rec.Body.String(), `"total_tokens":15`)
	require.NotContains(t, rec.Body.String(), "999")
}

func TestUsage_RejectsUnsupportedGroupBy(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodGet,
		"/v1/usage?group_by=planet", "", key))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"invalid_request"`)
}
