package apiserve

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st
}

func TestCreateAPIKey_ReturnsPlaintextOnce(t *testing.T) {
	st := newTestStore(t)

	created, plaintext, err := st.CreateAPIKey(context.Background(), CreateAPIKeyParams{
		Name:          "order-service",
		OwnerType:     "internal_service",
		AllowedAgents: []string{"main"},
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(plaintext, "pk_live_"))
	require.Equal(t, HashAPIKey(plaintext), created.KeyHash)
	require.NotEmpty(t, created.ID)
	require.True(t, created.Enabled)

	// The plaintext must never be persisted.
	var stored string
	require.NoError(t, st.db.QueryRow(
		`SELECT key_hash FROM api_keys WHERE id = ?`, created.ID,
	).Scan(&stored))
	require.NotEqual(t, plaintext, stored)
}

func TestLookupAPIKey_FindsCreatedKey(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	created, plaintext, err := st.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name:          "order-service",
		OwnerType:     "internal_service",
		AllowedAgents: []string{"main", "stock"},
		AllowedSkills: []string{"finance-news"},
		RateLimit:     "20/s",
	})
	require.NoError(t, err)

	got, err := st.LookupAPIKey(ctx, plaintext)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, []string{"main", "stock"}, got.AllowedAgents)
	require.Equal(t, []string{"finance-news"}, got.AllowedSkills)
	require.Equal(t, "20/s", got.RateLimit)
	require.True(t, got.Enabled)
}

func TestLookupAPIKey_UnknownKeyReturnsErrNotFound(t *testing.T) {
	st := newTestStore(t)

	_, err := st.LookupAPIKey(context.Background(), "pk_live_deadbeef")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLookupAPIKey_DisabledKeyReturnsErrKeyDisabled(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	created, plaintext, err := st.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "legacy-service", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	_, err = st.db.ExecContext(ctx, `UPDATE api_keys SET enabled = 0 WHERE id = ?`, created.ID)
	require.NoError(t, err)

	_, err = st.LookupAPIKey(ctx, plaintext)
	require.ErrorIs(t, err, ErrKeyDisabled)
}

func TestCreateAPIKey_NilAllowedListsMeanUnrestricted(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	created, plaintext, err := st.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "wildcard-service", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	got, err := st.LookupAPIKey(ctx, plaintext)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Empty(t, got.AllowedAgents)
	require.Empty(t, got.AllowedSkills)
}

func TestListAPIKeys_ReturnsCreatedKeys(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	created, _, err := st.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name:          "order-service",
		OwnerType:     "internal_service",
		AllowedAgents: []string{"main"},
	})
	require.NoError(t, err)

	keys, err := st.ListAPIKeys(ctx)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, created.ID, keys[0].ID)
	require.Equal(t, "order-service", keys[0].Name)
	require.Equal(t, []string{"main"}, keys[0].AllowedAgents)
	require.True(t, keys[0].Enabled)
}
