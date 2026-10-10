package apiserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHealth_ReturnsOK(t *testing.T) {
	srv := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

func TestReady_ReflectsReadyFlag(t *testing.T) {
	srv := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	srv.SetReady(true)

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

func TestShutdown_IsIdempotent(t *testing.T) {
	srv := newTestServer(t)

	require.NoError(t, srv.Shutdown(context.Background()))
	require.NoError(t, srv.Shutdown(context.Background()))
}
