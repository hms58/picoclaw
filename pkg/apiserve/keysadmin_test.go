package apiserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetAPIKeyEnabled_TogglesAndRejectsUnknown(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	created, plaintext, err := st.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)

	require.ErrorIs(t, st.SetAPIKeyEnabled(ctx, "key_nope", false), ErrNotFound)

	require.NoError(t, st.SetAPIKeyEnabled(ctx, created.ID, false))
	_, err = st.LookupAPIKey(ctx, plaintext)
	require.ErrorIs(t, err, ErrKeyDisabled)

	require.NoError(t, st.SetAPIKeyEnabled(ctx, created.ID, true))
	_, err = st.LookupAPIKey(ctx, plaintext)
	require.NoError(t, err)
}

func TestRotateAPIKey_ReplacesAndDisablesOriginal(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	original, originalPlaintext, err := st.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name:          "order-service",
		OwnerType:     "internal_service",
		AllowedAgents: []string{"main"},
		AllowedSkills: []string{"finance-news"},
		RateLimit:     "20/s,8",
	})
	require.NoError(t, err)

	replacement, newPlaintext, err := st.RotateAPIKey(ctx, original.ID)
	require.NoError(t, err)
	require.NotEqual(t, original.ID, replacement.ID)
	require.NotEqual(t, originalPlaintext, newPlaintext)

	// 新 key 继承旧 key 的全部属性
	require.Equal(t, "order-service", replacement.Name)
	require.Equal(t, []string{"main"}, replacement.AllowedAgents)
	require.Equal(t, []string{"finance-news"}, replacement.AllowedSkills)
	require.Equal(t, "20/s,8", replacement.RateLimit)
	require.True(t, replacement.Enabled)

	// 旧的已停用、新的可用
	_, err = st.LookupAPIKey(ctx, originalPlaintext)
	require.ErrorIs(t, err, ErrKeyDisabled)
	_, err = st.LookupAPIKey(ctx, newPlaintext)
	require.NoError(t, err)
}

func TestRotateAPIKey_UnknownReturnsErrNotFound(t *testing.T) {
	st := newTestStore(t)

	_, _, err := st.RotateAPIKey(context.Background(), "key_nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGetAPIKeyByID_ReturnsDisabledKeys(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	created, _, err := st.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name: "svc", OwnerType: "internal_service",
	})
	require.NoError(t, err)
	require.NoError(t, st.SetAPIKeyEnabled(ctx, created.ID, false))

	got, err := st.GetAPIKeyByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.False(t, got.Enabled)

	_, err = st.GetAPIKeyByID(ctx, "key_nope")
	require.ErrorIs(t, err, ErrNotFound)
}
