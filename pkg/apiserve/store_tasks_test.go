package apiserve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func createTestTask(t *testing.T, st *Store, convID string) *Task {
	t.Helper()
	task, err := st.CreateTask(context.Background(), CreateTaskParams{
		APIKeyID:       "key_1",
		AgentID:        "main",
		UserID:         "u_1",
		ConversationID: convID,
		SessionKey:     "sk_v1_aaa",
		Request:        messagesRequest{AgentID: "main", Message: "hello"},
		CallbackURL:    "http://127.0.0.1:1/hook",
	})
	require.NoError(t, err)
	return task
}

func TestCreateTask_StartsQueued(t *testing.T) {
	st := newTestStore(t)

	task := createTestTask(t, st, "conv_a")
	require.Equal(t, TaskQueued, task.Status)
	require.Contains(t, task.ID, "task_")
	require.Equal(t, "main", task.Request.AgentID)

	got, err := st.GetTask(context.Background(), task.ID)
	require.NoError(t, err)
	require.Equal(t, task.ID, got.ID)
	require.Equal(t, TaskQueued, got.Status)
}

func TestGetTask_UnknownReturnsErrNotFound(t *testing.T) {
	st := newTestStore(t)

	_, err := st.GetTask(context.Background(), "task_nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestClaimTasks_MovesToRunningOnce(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	createTestTask(t, st, "conv_a")
	createTestTask(t, st, "conv_b")

	claimed, err := st.ClaimTasks(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, TaskRunning, claimed[0].Status)

	again, err := st.ClaimTasks(ctx, 10)
	require.NoError(t, err)
	require.Len(t, again, 1, "the already claimed task must not be handed out twice")
	require.NotEqual(t, claimed[0].ID, again[0].ID)
}

func TestFinishTask_StoresResult(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	task := createTestTask(t, st, "conv_a")

	require.NoError(t, st.FinishTask(ctx, task.ID, TaskSucceeded, "", "",
		&messagesResponse{ConversationID: "conv_a", Content: "done"}))

	got, err := st.GetTask(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, TaskSucceeded, got.Status)
	require.NotNil(t, got.Result)
	require.Equal(t, "done", got.Result.Content)
	require.False(t, got.FinishedAt.IsZero())
}

func TestMarkStaleRunningTasks_OnlyTouchesOldRunning(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	stale := createTestTask(t, st, "conv_old")
	_, err := st.ClaimTasks(ctx, 10)
	require.NoError(t, err)
	_, err = st.db.ExecContext(ctx,
		`UPDATE tasks SET started_at = ? WHERE id = ?`, time.Now().Add(-2*time.Hour).Unix(), stale.ID)
	require.NoError(t, err)

	fresh := createTestTask(t, st, "conv_new")
	claimed, err := st.ClaimTasks(ctx, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, fresh.ID, claimed[0].ID,
		"the stale task is running, not queued, so only the fresh one is claimable")

	recovered, err := st.MarkStaleRunningTasks(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	require.Equal(t, stale.ID, recovered[0].ID)
	require.Equal(t, TaskFailed, recovered[0].Status)
	require.Equal(t, "interrupted", recovered[0].ErrorCode)

	got, err := st.GetTask(ctx, fresh.ID)
	require.NoError(t, err)
	require.Equal(t, TaskRunning, got.Status, "the fresh running task must not be swept")
}

func TestRequeueTask_ReturnsToQueue(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	task := createTestTask(t, st, "conv_a")

	claimed, err := st.ClaimTasks(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, TaskRunning, claimed[0].Status)

	require.NoError(t, st.RequeueTask(ctx, task.ID))

	got, err := st.GetTask(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, TaskQueued, got.Status)
	require.True(t, got.StartedAt.IsZero(), "a requeued task must look unclaimed again")

	// And it must be claimable once more.
	again, err := st.ClaimTasks(ctx, 1)
	require.NoError(t, err)
	require.Len(t, again, 1)
	require.Equal(t, task.ID, again[0].ID)
}

// 取消必须稳定生效：终态首次写入即胜，不被随后完成的 worker 覆盖。
func TestFinishTask_DoesNotClobberATerminalState(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	task := createTestTask(t, st, "conv_a")

	claimed, err := st.ClaimTasks(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	require.NoError(t, st.MarkTaskCanceled(ctx, task.ID))

	// A worker finishing afterwards must not overwrite `canceled`.
	require.NoError(t, st.FinishTask(ctx, task.ID, TaskSucceeded, "", "",
		&messagesResponse{Content: "late"}))

	got, err := st.GetTask(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, TaskCanceled, got.Status)

	// The reverse order keeps the first terminal state too.
	other := createTestTask(t, st, "conv_b")
	_, err = st.ClaimTasks(ctx, 1)
	require.NoError(t, err)
	require.NoError(t, st.FinishTask(ctx, other.ID, TaskSucceeded, "", "", nil))
	require.NoError(t, st.MarkTaskCanceled(ctx, other.ID))

	gotOther, err := st.GetTask(ctx, other.ID)
	require.NoError(t, err)
	require.Equal(t, TaskSucceeded, gotOther.Status)
}

func TestAPIKeyHashByID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	created, plaintext, err := st.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	got, err := st.APIKeyHashByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, HashAPIKey(plaintext), got)

	_, err = st.APIKeyHashByID(ctx, "key_nope")
	require.ErrorIs(t, err, ErrNotFound)
}
