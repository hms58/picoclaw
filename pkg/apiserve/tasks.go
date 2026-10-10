package apiserve

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// TaskRunner executes queued tasks and delivers their callbacks.
type TaskRunner struct {
	srv   *Server
	queue chan *Task

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// startMu serialises worker registration against Stop's wait. Without it
	// wg.Add (in Start) can run concurrently with wg.Wait (in Stop), which is
	// undefined for sync.WaitGroup: Stop could return before the workers were
	// registered, leaving them running after the pool reported itself stopped.
	startMu sync.Mutex

	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc

	client *http.Client
}

// NewTaskRunner creates a runner bound to a server.
func NewTaskRunner(srv *Server) *TaskRunner {
	return &TaskRunner{
		srv:     srv,
		queue:   make(chan *Task, 256),
		stopCh:  make(chan struct{}),
		cancels: make(map[string]context.CancelFunc),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Start recovers stale tasks and launches the worker pool. It returns
// immediately; workers stop when Stop is called.
func (r *TaskRunner) Start(ctx context.Context) {
	// Held for the whole registration phase so Stop cannot call wg.Wait before
	// every wg.Add has happened.
	r.startMu.Lock()
	defer r.startMu.Unlock()

	select {
	case <-r.stopCh:
		return // already stopped; do not register workers nobody will wait for
	default:
	}

	if stale, err := r.srv.store.MarkStaleRunningTasks(ctx, time.Hour); err != nil {
		logger.WarnCF("apiserve", "failed to recover stale tasks", map[string]any{"error": err.Error()})
	} else if len(stale) > 0 {
		logger.InfoCF("apiserve", "marked stale tasks as interrupted",
			map[string]any{"count": len(stale)})
	}

	workers := r.srv.settings.AsyncWorkers
	if workers <= 0 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		r.wg.Add(1)
		go r.worker()
	}

	// Poll for tasks created by other processes or left queued at startup.
	r.wg.Add(1)
	go r.poller()
}

// Stop waits for in-flight work and shuts the workers down.
func (r *TaskRunner) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })

	// Let Start finish registering workers before waiting on them: wg.Add racing
	// wg.Wait is undefined, and Stop returning early would leave workers running
	// behind a "stopped" pool.
	r.startMu.Lock()
	r.startMu.Unlock()

	r.wg.Wait()
}

// Enqueue hands a task to a worker.
func (r *TaskRunner) Enqueue(task *Task) {
	select {
	case r.queue <- task:
	case <-r.stopCh:
	}
}

// Cancel requests cancellation of a running task.
func (r *TaskRunner) Cancel(taskID string) bool {
	r.cancelMu.Lock()
	cancel, ok := r.cancels[taskID]
	r.cancelMu.Unlock()
	if ok {
		cancel()
		return true
	}
	return false
}

func (r *TaskRunner) poller() {
	defer r.wg.Done()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			capacity := cap(r.queue) - len(r.queue)
			if capacity <= 0 {
				continue
			}
			claimed, err := r.srv.store.ClaimTasks(context.Background(), capacity)
			if err != nil {
				logger.WarnCF("apiserve", "failed to claim tasks", map[string]any{"error": err.Error()})
				continue
			}
			for _, task := range claimed {
				r.Enqueue(task)
			}
		}
	}
}

func (r *TaskRunner) worker() {
	defer r.wg.Done()

	for {
		select {
		case <-r.stopCh:
			return
		case task := <-r.queue:
			r.runTask(task)
		}
	}
}

func (r *TaskRunner) runTask(task *Task) {
	ctx, cancel := context.WithTimeout(context.Background(), r.srv.settings.MaxTimeout)
	defer cancel()

	r.cancelMu.Lock()
	r.cancels[task.ID] = cancel
	r.cancelMu.Unlock()
	defer func() {
		r.cancelMu.Lock()
		delete(r.cancels, task.ID)
		r.cancelMu.Unlock()
	}()

	release, err := r.srv.gate.TryAcquire(task.ConversationID)
	if errors.Is(err, ErrConversationBusy) {
		// Another turn (sync or async) owns this conversation. The task queue
		// exists to serialize turns, so wait rather than fail: a caller of an
		// async endpoint gets no HTTP response to receive a 409 in.
		//
		// Bounded so a permanently busy conversation cannot keep a task alive
		// forever: past MaxTimeout the task gives up loudly.
		if time.Since(task.CreatedAt) > r.srv.settings.MaxTimeout {
			r.finishTask(ctx, task.ID, TaskFailed, "conversation_busy",
				"conversation stayed busy for longer than the task timeout", nil)
			return
		}
		if requeueErr := r.srv.store.RequeueTask(ctx, task.ID); requeueErr != nil {
			logger.WarnCF("apiserve", "failed to requeue busy task", map[string]any{
				"task_id": task.ID, "error": requeueErr.Error(),
			})
			r.finishTask(ctx, task.ID, TaskFailed, "internal_error", requeueErr.Error(), nil)
		}
		return
	}
	if err != nil {
		r.finishTask(ctx, task.ID, TaskFailed, "internal_error", err.Error(), nil)
		return
	}
	defer release()

	ctx = WithMeterContext(ctx, MeterContext{
		APIKeyID:       task.APIKeyID,
		AgentID:        task.AgentID,
		UserID:         task.UserID,
		ConversationID: task.ConversationID,
		SessionKey:     task.SessionKey,
		RequestID:      task.ID,
	})

	started := time.Now()
	// 同样必须用 Task 12 的补丁入口：ProcessDirectWithChannel 会忽略 task.AgentID。
	content, err := r.srv.loop.ProcessDirectWithAgent(
		ctx, task.Request.Message, task.SessionKey, "api", task.ConversationID, task.AgentID,
	)
	if err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			r.finishTask(ctx, task.ID, TaskTimeout, codeAgentTimeout, err.Error(), nil)
		case errors.Is(context.Cause(ctx), context.Canceled):
			// Same reason as finishTask: this ctx is the one that was just
			// cancelled, so an inherited write would never reach the database.
			if cancelErr := r.srv.store.MarkTaskCanceled(context.WithoutCancel(ctx), task.ID); cancelErr != nil {
				logger.ErrorCF("apiserve", "failed to mark task canceled", map[string]any{
					"task_id": task.ID, "error": cancelErr.Error(),
				})
			}
		case errors.Is(err, agent.ErrAgentNotFound):
			r.finishTask(ctx, task.ID, TaskFailed, codeAgentNotFound, err.Error(), nil)
		default:
			r.finishTask(ctx, task.ID, TaskFailed, "agent_error", err.Error(), nil)
		}
		return
	}

	result := &messagesResponse{
		ConversationID: task.ConversationID,
		MessageID:      fmt.Sprintf("msg_%s", task.ID),
		AgentID:        task.AgentID,
		Content:        content,
		SessionKey:     task.SessionKey,
		DurationMS:     time.Since(started).Milliseconds(),
	}
	// Same detachment as finishTask: a Cancel arriving just after the turn
	// returned must not be able to swallow the success write. FinishTask is
	// first-terminal-wins, so if cancel won the race this write no-ops and the
	// callback below is skipped by its own status check.
	if err := r.srv.store.FinishTask(context.WithoutCancel(ctx), task.ID, TaskSucceeded, "", "", result); err != nil {
		logger.ErrorCF("apiserve", "failed to persist task result", map[string]any{
			"task_id": task.ID, "error": err.Error(),
		})
	}

	if task.CallbackURL != "" {
		r.deliverCallback(ctx, task, result)
	}
}

// finishTask records a terminal state and logs a failure instead of dropping it:
// a silent failure leaves the task `running` until the stale sweep an hour later,
// with nothing in the logs to explain it.
//
// The write must not inherit the turn's deadline or cancellation. On the timeout
// path the turn context is already expired, database/sql would then skip the
// statement entirely, and the documented `timeout` state would be unreachable —
// exactly the silent failure this function exists to prevent.
func (r *TaskRunner) finishTask(
	ctx context.Context,
	id, status, errorCode, errorMessage string,
	result *messagesResponse,
) {
	ctx = context.WithoutCancel(ctx)
	if err := r.srv.store.FinishTask(ctx, id, status, errorCode, errorMessage, result); err != nil {
		logger.ErrorCF("apiserve", "failed to record task state", map[string]any{
			"task_id": id, "status": status, "error": err.Error(),
		})
	}
}

// deliverCallback posts the result with an HMAC signature. The signing key is
// the api key hash, which the caller can derive as sha256(its own api key).
func (r *TaskRunner) deliverCallback(ctx context.Context, task *Task, result *messagesResponse) {
	ctx = context.WithoutCancel(ctx)

	// Re-read the row before announcing success. A Cancel can land between the
	// success write and here; the callback body hardcodes status "succeeded", so
	// without this check a task the caller just cancelled would be reported as
	// having succeeded. Fail closed: a dropped best-effort callback is far
	// cheaper than a false success, and the result stays readable via
	// GET /v1/tasks/{id}.
	current, readErr := r.srv.store.GetTask(ctx, task.ID)
	if readErr != nil {
		logger.WarnCF("apiserve", "skipped callback, could not confirm task status", map[string]any{
			"task_id": task.ID, "error": readErr.Error(),
		})
		return
	}
	if current.Status != TaskSucceeded {
		logger.InfoCF("apiserve", "skipped callback, task is no longer succeeded", map[string]any{
			"task_id": task.ID, "status": current.Status,
		})
		return
	}

	body, err := json.Marshal(map[string]any{
		"task_id": task.ID,
		"status":  TaskSucceeded,
		"result":  result,
	})
	if err != nil {
		logger.WarnCF("apiserve", "failed to encode callback", map[string]any{"error": err.Error()})
		return
	}

	signingKey, err := r.srv.store.APIKeyHashByID(ctx, task.APIKeyID)
	if err != nil {
		logger.WarnCF("apiserve", "failed to load callback signing key", map[string]any{
			"task_id": task.ID, "error": err.Error(),
		})
		return
	}

	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	const maxAttempts = 5
	backoff := 100 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, reqErr := http.NewRequestWithContext(
			context.WithoutCancel(ctx), http.MethodPost, task.CallbackURL, bytes.NewReader(body),
		)
		if reqErr != nil {
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Task-Id", task.ID)
		req.Header.Set("X-Signature", signature)

		resp, doErr := r.client.Do(req)
		if doErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				_ = r.srv.store.SetCallbackStatus(ctx, task.ID, CallbackDelivered, attempt)
				return
			}
		}

		if attempt < maxAttempts {
			select {
			case <-time.After(backoff):
			case <-r.stopCh:
				_ = r.srv.store.SetCallbackStatus(ctx, task.ID, CallbackFailed, attempt)
				return
			}
			backoff *= 2
		}
	}

	_ = r.srv.store.SetCallbackStatus(ctx, task.ID, CallbackFailed, maxAttempts)
	logger.WarnCF("apiserve", "callback delivery failed", map[string]any{"task_id": task.ID})
}
