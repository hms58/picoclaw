package apiserve

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccessLog_EmitsRequiredFields(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	records := make(chan AccessRecord, 4)
	srv.accessLog = func(rec AccessRecord) { records <- rec }

	rec := httptest.NewRecorder()
	req := newAuthedRequest(t, http.MethodPost, "/v1/messages",
		`{"agent_id":"main","message":"hello"}`, key)
	req.Header.Set("X-Request-Id", "req_test_1")
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	select {
	case got := <-records:
		require.Equal(t, "req_test_1", got.RequestID)
		require.Equal(t, http.MethodPost, got.Method)
		require.Equal(t, "/v1/messages", got.Path)
		require.Equal(t, http.StatusOK, got.StatusCode)
		require.NotEmpty(t, got.APIKeyID)
		require.Equal(t, "main", got.AgentID)
		require.Contains(t, got.ConversationID, "conv_")
		// spec §7.3 lists `usage` as part of the request log line.
		require.Equal(t, 10, got.InputTokens)
		require.Equal(t, 5, got.OutputTokens)
	case <-time.After(time.Second):
		t.Fatal("no access record emitted")
	}
}

// Without an X-Request-Id header the server must still use ONE id everywhere:
// requestIDFrom falls back to a fresh random id per call, so the access log, the
// error envelope and usage_records would otherwise be uncorrelatable. This test
// deliberately sends no header — the other tests set one, which masked the bug.
func TestAccessLog_GeneratedRequestIDMatchesTheResponseEnvelope(t *testing.T) {
	srv, _, key := newMessageServer(t, nil)

	records := make(chan AccessRecord, 4)
	srv.accessLog = func(rec AccessRecord) { records <- rec }

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodPost, "/v1/messages",
		`{"agent_id":"nope","message":"hello"}`, key))

	require.Equal(t, http.StatusNotFound, rec.Code)

	select {
	case got := <-records:
		require.NotEmpty(t, got.RequestID)
		require.Contains(t, rec.Body.String(), got.RequestID,
			"the access log's request_id must be the one in the error envelope")
	case <-time.After(time.Second):
		t.Fatal("no access record emitted")
	}
}

func TestAccessLog_IsEmittedForRejectedRequests(t *testing.T) {
	srv := newTestServer(t)

	records := make(chan AccessRecord, 4)
	srv.accessLog = func(rec AccessRecord) { records <- rec }

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agents", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	select {
	case got := <-records:
		require.Equal(t, http.StatusUnauthorized, got.StatusCode)
	case <-time.After(time.Second):
		t.Fatal("no access record emitted for rejected request")
	}
}
