package apiserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/config"
)

// twoAgentConfig registers two agents so allowed_agents filtering is visible.
func twoAgentConfig(t *testing.T) *config.Config {
	cfg := testConfig(t)
	cfg.Agents.List = []config.AgentConfig{
		{ID: "main", Default: true, Workspace: t.TempDir()},
		{ID: "stock-research", Workspace: t.TempDir()},
	}
	return cfg
}

func TestAgents_ListReturnsRegistryIDs(t *testing.T) {
	srv := newTestServer(t)
	_, plaintext, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodGet, "/v1/agents", "", plaintext))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"main"`)
}

func TestAgents_FiltersByAllowedAgents(t *testing.T) {
	srv, err := New(Options{
		Home:     t.TempDir(),
		Config:   twoAgentConfig(t),
		Provider: &fakeProvider{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	_, plaintext, err := srv.store.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
		AllowedAgents: []string{"stock-research"},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newAuthedRequest(t, http.MethodGet, "/v1/agents", "", plaintext))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"stock-research"`)
	require.NotContains(t, rec.Body.String(), `"main"`)
}
