package apiserve

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// getStatus issues a GET and returns the status code, or 0 when the request
// could not be made at all (server not listening yet).
func getStatus(url string) int {
	resp, err := http.Get(url) //nolint:noctx // short-lived probe in a test
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// runningServer is a Server driven through its real Start path, listening on an
// OS-assigned port.
type runningServer struct {
	srv  *Server
	base string
	key  string
}

// startTestServer binds a listener, starts the server on it in the background,
// and registers cleanup that asserts Start returned cleanly.
//
// The listener is created by the test and injected, so the address is known
// before Start is called: no polling and no accessor on Server.
func startTestServer(t *testing.T, opts Options) *runningServer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	// Registered first so it runs last: Start's shutdown closes the listener
	// normally, and this only bites when New fails before that cleanup exists.
	t.Cleanup(func() { _ = ln.Close() })

	opts.Listener = ln
	if opts.Home == "" {
		opts.Home = t.TempDir()
	}
	if opts.Config == nil {
		opts.Config = testConfig(t)
	}
	if opts.Provider == nil {
		opts.Provider = &fakeProvider{}
	}

	srv, err := New(opts)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Start returned %v, want nil after cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Start did not return after the context was cancelled")
		}
	})

	_, key, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "start-test", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	return &runningServer{srv: srv, base: "http://" + ln.Addr().String(), key: key}
}

// Server.Start must do two things that no other test covers: serve HTTP on the
// real listener, and start the async task pool. Without the s.tasks.Start(ctx)
// call in Start, nothing ever claims a queued task — the poller lives in
// TaskRunner.Start — so the task below stays queued forever and this test times
// out. That is exactly the production bug the Task 8 review found.
func TestStart_ServesHTTPAndClaimsQueuedTasks(t *testing.T) {
	run := startTestServer(t, Options{})

	require.Eventually(t, func() bool {
		return getStatus(run.base+"/health") == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "the HTTP server never became healthy")

	// /ready only flips once Start has bound the listener.
	require.Equal(t, http.StatusOK, getStatus(run.base+"/ready"))

	body := strings.NewReader(`{"agent_id":"main","message":"hello"}`)
	req, err := http.NewRequest(http.MethodPost, run.base+"/v1/tasks", body)
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

	// The load-bearing assertion: a task accepted over real HTTP is actually
	// claimed by the pool Server.Start started.
	require.Eventually(t, func() bool {
		task, getErr := run.srv.store.GetTask(context.Background(), taskID)
		if getErr != nil {
			return false
		}
		return task.Status != TaskQueued && task.Status != TaskRunning
	}, 5*time.Second, 20*time.Millisecond,
		"the task pool never claimed the queued task: is TaskRunner.Start called by Server.Start?")

	task, err := run.srv.store.GetTask(context.Background(), taskID)
	require.NoError(t, err)
	require.Equal(t, TaskSucceeded, task.Status)
	require.NotNil(t, task.Result)
	require.Equal(t, "Mock response", task.Result.Content)
}

// Cancelling the context is a clean shutdown, and the port must be released.
func TestStart_ShutsDownCleanlyAndReleasesThePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()

	srv, err := New(Options{
		Home:     t.TempDir(),
		Config:   testConfig(t),
		Provider: &fakeProvider{},
		Listener: ln,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	require.Eventually(t, func() bool {
		return getStatus("http://"+addr+"/health") == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "cancelling the context is a clean shutdown, not an error")
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the context was cancelled")
	}

	require.Eventually(t, func() bool {
		rebound, listenErr := net.Listen("tcp", addr)
		if listenErr != nil {
			return false
		}
		_ = rebound.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "the listening port was never released")
}

// The bind target and the worker count must be configurable: spec §2 promises
// api.host/api.port (default 127.0.0.1:18900, "给内网用改 0.0.0.0") and §4
// promises api.async_workers. Before this seam existed the server could only
// ever listen on the hardcoded 127.0.0.1:18900, which makes the documented
// internal-network deployment impossible.
//
// No Listener is injected here on purpose: with an injected listener the
// configured Host/Port never reach a socket, so the address assertion would be
// vacuous. httpSrv.Addr is exactly what Start hands to net.Listen.
func TestOptions_SettingsOverrideTheBindTargetAndWorkers(t *testing.T) {
	custom := DefaultSettings(t.TempDir())
	custom.Host = "0.0.0.0"
	custom.Port = 12345
	custom.AsyncWorkers = 7

	srv, err := New(Options{
		Home:     t.TempDir(),
		Config:   testConfig(t),
		Provider: &fakeProvider{},
		Settings: &custom,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	require.Equal(t, "0.0.0.0:12345", srv.httpSrv.Addr,
		"Settings.Host/Port must reach the address Start listens on")
	require.Equal(t, 7, srv.settings.AsyncWorkers)
}

// A partial Settings must inherit the defaults for every field it leaves zero.
// A wholesale replacement would zero MaxTimeout — making context.WithTimeout(0,
// …) expire every turn immediately — and take DBPath with it, splitting the
// server's database from the one the `keys` CLI derives from the same home.
func TestOptions_PartialSettingsInheritTheDefaults(t *testing.T) {
	home := t.TempDir()
	defaults := DefaultSettings(home)

	srv, err := New(Options{
		Home:     home,
		Config:   testConfig(t),
		Provider: &fakeProvider{},
		Settings: &ServerSettings{Port: 12346}, // every other field left zero
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	require.Equal(t, defaults.DBPath, srv.settings.DBPath, "DBPath must stay in the given home")
	require.Equal(t, defaults.MaxTimeout, srv.settings.MaxTimeout,
		"a zero MaxTimeout would expire every turn immediately")
	require.Equal(t, defaults.DefaultTimeout, srv.settings.DefaultTimeout)
	require.Equal(t, defaults.AsyncWorkers, srv.settings.AsyncWorkers)
	require.Equal(t, defaults.Host, srv.settings.Host)
	require.Equal(t, 12346, srv.settings.Port)
}
