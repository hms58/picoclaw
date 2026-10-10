package apiserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMessages_IdempotencyKeyReplaysWithoutCallingAgent(t *testing.T) {
	srv, provider, key := newMessageServer(t, nil)

	first := httptest.NewRecorder()
	req := newAuthedRequest(t, http.MethodPost, "/v1/messages",
		`{"agent_id":"main","message":"hello"}`, key)
	req.Header.Set("Idempotency-Key", "idem-xyz")
	srv.Handler().ServeHTTP(first, req)
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, 1, provider.calls)

	second := httptest.NewRecorder()
	req2 := newAuthedRequest(t, http.MethodPost, "/v1/messages",
		`{"agent_id":"main","message":"hello"}`, key)
	req2.Header.Set("Idempotency-Key", "idem-xyz")
	srv.Handler().ServeHTTP(second, req2)

	require.Equal(t, http.StatusOK, second.Code)
	require.Equal(t, first.Body.String(), second.Body.String())
	require.Equal(t, 1, provider.calls, "replay must not call the agent again")
	require.Equal(t, "true", second.Header().Get("Idempotent-Replay"))
}

func TestMessages_RateLimitReturns429(t *testing.T) {
	srv, _, _ := newMessageServer(t, nil)
	_, key, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "burst", OwnerType: "internal_service", RateLimit: "1/s,1",
	})
	require.NoError(t, err)

	first := postMessages(t, srv, key, `{"agent_id":"main","message":"a"}`)
	require.Equal(t, http.StatusOK, first.Code)

	second := postMessages(t, srv, key, `{"agent_id":"main","message":"b"}`)
	require.Equal(t, http.StatusTooManyRequests, second.Code)
	require.Contains(t, second.Body.String(), `"code":"rate_limited"`)
}
