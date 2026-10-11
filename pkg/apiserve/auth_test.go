package apiserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuth_MissingHeaderReturns401(t *testing.T) {
	srv := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agents", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"unauthorized"`)
}

func TestAuth_UnknownKeyReturns401(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer pk_live_nope")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"unauthorized"`)
}

func TestAuth_DisabledKeyReturns401(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	created, plaintext, err := srv.store.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "legacy", OwnerType: "internal_service",
	})
	require.NoError(t, err)
	_, err = srv.store.db.ExecContext(ctx, `UPDATE api_keys SET enabled = 0 WHERE id = ?`, created.ID)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestPrincipal_AllowsAgent(t *testing.T) {
	require.True(t, (&Principal{}).AllowsAgent("anything"), "empty list means unrestricted")
	require.True(t, (&Principal{AllowedAgents: []string{"main"}}).AllowsAgent("main"))
	require.False(t, (&Principal{AllowedAgents: []string{"main"}}).AllowsAgent("stock"))
}
