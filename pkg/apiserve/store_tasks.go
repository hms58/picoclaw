package apiserve

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Task lifecycle states.
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskCanceled  = "canceled"
	TaskTimeout   = "timeout"
)

// Callback delivery states.
const (
	CallbackPending   = "pending"
	CallbackDelivered = "delivered"
	CallbackFailed    = "failed"
)

// Task is one asynchronous agent invocation.
type Task struct {
	ID               string
	Status           string
	APIKeyID         string
	AgentID          string
	UserID           string
	ConversationID   string
	SessionKey       string
	Request          messagesRequest
	Result           *messagesResponse
	ErrorCode        string
	ErrorMessage     string
	CallbackURL      string
	CallbackStatus   string
	CallbackAttempts int
	CreatedAt        time.Time
	StartedAt        time.Time
	FinishedAt       time.Time
}

// CreateTaskParams describes a task to enqueue.
type CreateTaskParams struct {
	APIKeyID       string
	AgentID        string
	UserID         string
	ConversationID string
	SessionKey     string
	Request        messagesRequest
	CallbackURL    string
}

// CreateTask inserts a queued task.
func (s *Store) CreateTask(ctx context.Context, p CreateTaskParams) (*Task, error) {
	id, err := newID("task")
	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(p.Request)
	if err != nil {
		return nil, fmt.Errorf("encode task request: %w", err)
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks
			(id, status, api_key_id, agent_id, user_id, conversation_id, session_key,
			 request_json, callback_url, callback_status, callback_attempts, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		id, TaskQueued, p.APIKeyID, p.AgentID, p.UserID, p.ConversationID, p.SessionKey,
		string(raw), p.CallbackURL, CallbackPending, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}

	return s.GetTask(ctx, id)
}

// GetTask loads a task by id.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, status, api_key_id, agent_id, user_id, conversation_id, session_key,
		       request_json, result_json, error_code, error_message,
		       callback_url, callback_status, callback_attempts,
		       created_at, started_at, finished_at
		FROM tasks WHERE id = ?`, id)

	var (
		task      Task
		requestJS string
		resultJS  string
		createdAt int64
		startedAt int64
		finished  int64
	)
	err := row.Scan(
		&task.ID, &task.Status, &task.APIKeyID, &task.AgentID, &task.UserID,
		&task.ConversationID, &task.SessionKey, &requestJS, &resultJS,
		&task.ErrorCode, &task.ErrorMessage, &task.CallbackURL,
		&task.CallbackStatus, &task.CallbackAttempts,
		&createdAt, &startedAt, &finished,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load task: %w", err)
	}

	if err := json.Unmarshal([]byte(requestJS), &task.Request); err != nil {
		return nil, fmt.Errorf("decode task request: %w", err)
	}
	if resultJS != "" {
		var result messagesResponse
		if err := json.Unmarshal([]byte(resultJS), &result); err != nil {
			return nil, fmt.Errorf("decode task result: %w", err)
		}
		task.Result = &result
	}

	task.CreatedAt = time.Unix(createdAt, 0)
	if startedAt > 0 {
		task.StartedAt = time.Unix(startedAt, 0)
	}
	if finished > 0 {
		task.FinishedAt = time.Unix(finished, 0)
	}
	return &task, nil
}

// ClaimTasks atomically moves up to limit queued tasks to running.
func (s *Store) ClaimTasks(ctx context.Context, limit int) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM tasks WHERE status = ? ORDER BY created_at LIMIT ?`,
		TaskQueued, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select queued tasks: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan queued task: %w", scanErr)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate queued tasks: %w", err)
	}
	_ = rows.Close()

	claimed := make([]*Task, 0, len(ids))
	now := time.Now().Unix()
	for _, id := range ids {
		res, execErr := s.db.ExecContext(ctx,
			`UPDATE tasks SET status = ?, started_at = ? WHERE id = ? AND status = ?`,
			TaskRunning, now, id, TaskQueued,
		)
		if execErr != nil {
			return nil, fmt.Errorf("claim task %s: %w", id, execErr)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("claim task %s: %w", id, err)
		}
		if affected == 0 {
			continue
		}
		task, err := s.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, task)
	}
	return claimed, nil
}

// FinishTask records the terminal state of a task.
//
// The update is restricted to non-terminal states so the FIRST terminal
// transition wins. Without this guard a task cancelled while its turn was still
// running would be overwritten to `succeeded` by the worker that finishes
// afterwards, making cancellation silently ineffective.
func (s *Store) FinishTask(
	ctx context.Context,
	id, status, errorCode, errorMessage string,
	result *messagesResponse,
) error {
	var resultJS string
	if result != nil {
		raw, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode task result: %w", err)
		}
		resultJS = string(raw)
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?, result_json = ?, error_code = ?, error_message = ?, finished_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		status, resultJS, errorCode, errorMessage, time.Now().Unix(), id,
		TaskQueued, TaskRunning,
	)
	if err != nil {
		return fmt.Errorf("finish task: %w", err)
	}
	return nil
}

// MarkTaskCanceled cancels a task if it has not reached a terminal state.
func (s *Store) MarkTaskCanceled(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, finished_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		TaskCanceled, time.Now().Unix(), id, TaskQueued, TaskRunning,
	)
	if err != nil {
		return fmt.Errorf("cancel task: %w", err)
	}
	return nil
}

// MarkStaleRunningTasks fails tasks left running by a previous process.
// Tasks are never replayed: tool side effects must not run twice.
func (s *Store) MarkStaleRunningTasks(ctx context.Context, staleAfter time.Duration) ([]*Task, error) {
	cutoff := time.Now().Add(-staleAfter).Unix()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM tasks WHERE status = ? AND started_at > 0 AND started_at <= ?`,
		TaskRunning, cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("select stale tasks: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan stale task: %w", scanErr)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate stale tasks: %w", err)
	}
	_ = rows.Close()

	out := make([]*Task, 0, len(ids))
	for _, id := range ids {
		if err := s.FinishTask(ctx, id, TaskFailed, "interrupted",
			"process restarted while this task was running", nil); err != nil {
			return nil, err
		}
		task, err := s.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, nil
}

// RequeueTask returns a claimed task to the queue.
//
// Used when the conversation is still busy: the async surface serializes turns
// instead of rejecting them (the sync surface answers 409, but an async caller
// has no response to receive that 409). The poller's 200ms tick is the backoff,
// so requeueing cannot spin.
func (s *Store) RequeueTask(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, started_at = 0 WHERE id = ? AND status = ?`,
		TaskQueued, id, TaskRunning,
	)
	if err != nil {
		return fmt.Errorf("requeue task: %w", err)
	}
	return nil
}

// SetCallbackStatus records the outcome of a callback delivery attempt.
func (s *Store) SetCallbackStatus(ctx context.Context, id, status string, attempts int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET callback_status = ?, callback_attempts = ? WHERE id = ?`,
		status, attempts, id,
	)
	if err != nil {
		return fmt.Errorf("set callback status: %w", err)
	}
	return nil
}
