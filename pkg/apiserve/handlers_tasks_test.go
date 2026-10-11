package apiserve

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// waitForTerminal polls until the task leaves queued/running.
func waitForTerminal(t *testing.T, srv *Server, taskID string) *Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := srv.store.GetTask(context.Background(), taskID)
		require.NoError(t, err)
		if task.Status != TaskQueued && task.Status != TaskRunning {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s never reached a terminal state", taskID)
	return nil
}

func newTaskServer(t *testing.T) (*Server, *fakeProvider, string) {
	t.Helper()
	srv, provider, key := newMessageServer(t, nil)
	go srv.tasks.Start(context.Background())
	t.Cleanup(func() { srv.tasks.Stop() })
	return srv, provider, key
}

func TestTasks_CreateReturns202(t *testing.T) {
	srv, _, key := newTaskServer(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodPost, "/v1/tasks",
		`{"agent_id":"main","message":"hello"}`, key))

	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Contains(t, rec.Body.String(), `"task_id":"task_`)
	require.Contains(t, rec.Body.String(), `"status":"queued"`)
}

func TestTasks_CompletesAndIsIdempotentToRead(t *testing.T) {
	srv, provider, key := newTaskServer(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodPost, "/v1/tasks",
		`{"agent_id":"main","message":"hello"}`, key))
	require.Equal(t, http.StatusAccepted, rec.Code)

	task := waitForTerminal(t, srv, taskIDFrom(t, rec.Body.Bytes()))
	require.Equal(t, TaskSucceeded, task.Status)
	require.NotNil(t, task.Result)
	require.Equal(t, "Mock response", task.Result.Content)
	require.Equal(t, 1, provider.calls)

	first := httptest.NewRecorder()
	srv.Handler().ServeHTTP(first, newAuthedRequest(t, http.MethodGet, "/v1/tasks/"+task.ID, "", key))
	second := httptest.NewRecorder()
	srv.Handler().ServeHTTP(second, newAuthedRequest(t, http.MethodGet, "/v1/tasks/"+task.ID, "", key))

	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, first.Body.String(), second.Body.String())
	require.Equal(t, 1, provider.calls, "reading a task must not re-run it")
}

func TestTasks_UnknownReturns404(t *testing.T) {
	srv, _, key := newTaskServer(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodGet, "/v1/tasks/task_nope", "", key))

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"task_not_found"`)
}

func TestTasks_CallbackIsSignedWithKeyHash(t *testing.T) {
	srv, _, key := newTaskServer(t)

	received := make(chan string, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- r.Header.Get("X-Signature") + "|" + string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)

	body := `{"agent_id":"main","message":"hello","callback_url":"` + hook.URL + `"}`
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodPost, "/v1/tasks", body, key))
	require.Equal(t, http.StatusAccepted, rec.Code)

	waitForTerminal(t, srv, taskIDFrom(t, rec.Body.Bytes()))

	select {
	case got := <-received:
		parts := strings.SplitN(got, "|", 2)
		require.Len(t, parts, 2)

		mac := hmac.New(sha256.New, []byte(HashAPIKey(key)))
		mac.Write([]byte(parts[1]))
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		require.Equal(t, want, parts[0])
		require.Contains(t, parts[1], "Mock response")
	case <-time.After(5 * time.Second):
		t.Fatal("callback was never delivered")
	}
}

// Review Focus 5: an unreachable callback must not change the task outcome.
func TestTasks_UnreachableCallbackKeepsTaskSucceeded(t *testing.T) {
	srv, _, key := newTaskServer(t)

	body := `{"agent_id":"main","message":"hello","callback_url":"http://127.0.0.1:1/hook"}`
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodPost, "/v1/tasks", body, key))
	require.Equal(t, http.StatusAccepted, rec.Code)

	task := waitForTerminal(t, srv, taskIDFrom(t, rec.Body.Bytes()))
	require.Equal(t, TaskSucceeded, task.Status)
	require.NotNil(t, task.Result)

	// The delivery attempt may still be in flight; poll for the final status.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := srv.store.GetTask(context.Background(), task.ID)
		require.NoError(t, err)
		if current.CallbackStatus == CallbackFailed {
			require.Equal(t, TaskSucceeded, current.Status)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("callback status never became failed")
}

// 忙会话必须被等待而不是判死：同步端点可以回 409，但异步调用方没有响应可收，
// 所以队列的职责是串行化。没有 RequeueTask 时本用例会看到 failed/conversation_busy。
func TestTasks_BusyConversationWaitsInsteadOfFailing(t *testing.T) {
	srv := newTestServer(t)

	task, err := srv.store.CreateTask(context.Background(), CreateTaskParams{
		APIKeyID:       "key_1",
		AgentID:        "main",
		UserID:         "u_1",
		ConversationID: "conv_busy_wait",
		SessionKey:     "sk_v1_aaa",
		Request:        messagesRequest{AgentID: "main", Message: "hello"},
	})
	require.NoError(t, err)

	// Simulate a sync turn holding the conversation.
	release, err := srv.gate.TryAcquire("conv_busy_wait")
	require.NoError(t, err)

	go srv.tasks.Start(context.Background())
	t.Cleanup(func() { srv.tasks.Stop() })

	// Long enough for the poller and a worker to hit the busy gate at least once.
	time.Sleep(500 * time.Millisecond)

	held, err := srv.store.GetTask(context.Background(), task.ID)
	require.NoError(t, err)
	require.NotEqual(t, TaskFailed, held.Status, "忙会话不得把任务判死")
	require.Equal(t, TaskQueued, held.Status, "任务应回到队列等待")

	release()

	final := waitForTerminal(t, srv, task.ID)
	require.Equal(t, TaskSucceeded, final.Status)
}

func TestTasks_Cancel(t *testing.T) {
	srv := newTestServer(t)
	go srv.tasks.Start(context.Background())
	t.Cleanup(func() { srv.tasks.Stop() })

	created, key, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	// The task must belong to the caller: /v1/tasks/{id} enforces ownership, so a
	// task bound to another key正would (correctly) answer 404.
	task, err := srv.store.CreateTask(context.Background(), CreateTaskParams{
		APIKeyID:       created.ID,
		AgentID:        "main",
		UserID:         "u_1",
		ConversationID: "conv_cancel",
		SessionKey:     "sk_v1_aaa",
		Request:        messagesRequest{AgentID: "main", Message: "hello"},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodPost, "/v1/tasks/"+task.ID+"/cancel", "", key))

	require.Equal(t, http.StatusAccepted, rec.Code)
	got, err := srv.store.GetTask(context.Background(), task.ID)
	require.NoError(t, err)
	require.Equal(t, TaskCanceled, got.Status)
}
