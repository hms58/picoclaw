package apiserve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Regression tests for the three cross-key / terminal-state defects the
// whole-branch final review found. Each one fails on the code as it stood
// before the fix.
//
// Final-review Blocking 1: the Idempotency-Key header is caller-supplied, so
// keying the table on it alone lets one API key read another key's stored
// response body — including the agent's reply text and the session key.
func TestIdempotency_IsScopedPerAPIKey(t *testing.T) {
	srv, provider, mine := newMessageServer(t, nil)
	_, theirs, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "other", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	post := func(apiKey, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := newAuthedRequest(t, http.MethodPost, "/v1/messages", body, apiKey)
		req.Header.Set("Idempotency-Key", "shared-header-value")
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	first := post(mine, `{"agent_id":"main","message":"first"}`)
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, 1, provider.calls)

	second := post(theirs, `{"agent_id":"main","message":"second"}`)
	require.Equal(t, http.StatusOK, second.Code)
	require.NotEqual(t, first.Body.String(), second.Body.String(),
		"another API key must not receive the first key's stored response body")
	require.Equal(t, 2, provider.calls,
		"the second key's request must actually run instead of replaying someone else's response")
}

// Final-review Blocking 2: a task that exceeds MaxTimeout must reach the
// terminal `timeout` state. Before the fix the terminal write used the already
// expired turn context, so database/sql skipped the statement, the failure was
// only logged, and the task stayed `running` for the process lifetime.
func TestTasks_TimeoutReachesTheTerminalState(t *testing.T) {
	blocker := &blockingProvider{}
	blocker.block.Store(true)

	settings := DefaultSettings(t.TempDir())
	settings.MaxTimeout = 200 * time.Millisecond

	run := startTestServer(t, Options{Provider: blocker, Settings: &settings})

	req, err := http.NewRequest(http.MethodPost, run.base+"/v1/tasks",
		strings.NewReader(`{"agent_id":"main","message":"hello"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+run.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	taskID := taskIDFrom(t, raw)

	require.Eventually(t, func() bool {
		task, getErr := run.srv.store.GetTask(context.Background(), taskID)
		if getErr != nil {
			return false
		}
		return task.Status != TaskQueued && task.Status != TaskRunning
	}, 5*time.Second, 20*time.Millisecond,
		"a timed-out task must reach a terminal state, not stay running forever")

	task, err := run.srv.store.GetTask(context.Background(), taskID)
	require.NoError(t, err)
	require.Equal(t, TaskTimeout, task.Status,
		"the documented terminal state for a timed-out task is %q", TaskTimeout)
}

// Final-review Important: the sync response's usage block was summed by
// request_id alone, and request_id comes from the caller-supplied
// X-Request-Id header. Two keys sharing that header therefore saw each other's
// tokens.
func TestMessages_UsageSumIsScopedPerAPIKey(t *testing.T) {
	srv, _, mine := newMessageServer(t, nil)
	_, theirs, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "other", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	post := func(apiKey string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := newAuthedRequest(t, http.MethodPost, "/v1/messages",
			`{"agent_id":"main","message":"hello"}`, apiKey)
		req.Header.Set("X-Request-Id", "shared-request-id")
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	first := post(mine)
	require.Equal(t, http.StatusOK, first.Code)
	require.Contains(t, first.Body.String(), `"input_tokens":10`)

	second := post(theirs)
	require.Equal(t, http.StatusOK, second.Code)
	require.NotContains(t, second.Body.String(), `"input_tokens":20`,
		"a second key must not have the first key's tokens added to its usage")
	require.Contains(t, second.Body.String(), `"input_tokens":10`)
}
