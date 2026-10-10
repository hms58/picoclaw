package apiserve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIdempotency_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	_, found, err := st.GetIdempotentResponse(ctx, "key_1", "idem_1")
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, st.SaveIdempotentResponse(ctx, "key_1", "idem_1", []byte(`{"ok":true}`)))

	body, found, err := st.GetIdempotentResponse(ctx, "key_1", "idem_1")
	require.NoError(t, err)
	require.True(t, found)
	require.JSONEq(t, `{"ok":true}`, string(body))
}

// spec §4.6 的重放窗口是"24h 内"：超窗的记录不得再被重放。
func TestIdempotency_ExpiredKeyIsNotReplayed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, st.SaveIdempotentResponse(ctx, "key_1", "idem_old", []byte(`{"n":1}`)))
	_, err := st.db.ExecContext(ctx,
		`UPDATE idempotency SET created_at = ? WHERE key = ?`,
		time.Now().Add(-25*time.Hour).Unix(), idempotencyScope("key_1", "idem_old"))
	require.NoError(t, err)

	_, found, err := st.GetIdempotentResponse(ctx, "key_1", "idem_old")
	require.NoError(t, err)
	require.False(t, found, "超过 24h 的幂等记录不得再重放")
}

func TestIdempotency_SavePrunesExpiredRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, st.SaveIdempotentResponse(ctx, "key_1", "idem_old", []byte(`{"n":1}`)))
	_, err := st.db.ExecContext(ctx,
		`UPDATE idempotency SET created_at = ? WHERE key = ?`,
		time.Now().Add(-25*time.Hour).Unix(), idempotencyScope("key_1", "idem_old"))
	require.NoError(t, err)

	require.NoError(t, st.SaveIdempotentResponse(ctx, "key_1", "idem_new", []byte(`{"n":2}`)))

	var remaining int
	require.NoError(t, st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM idempotency WHERE key = ?`,
		idempotencyScope("key_1", "idem_old")).Scan(&remaining))
	require.Zero(t, remaining, "过期行应被顺带剪枝，避免表无界增长")
}

func TestIdempotency_SaveTwiceKeepsFirst(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, st.SaveIdempotentResponse(ctx, "key_1", "idem_1", []byte(`{"n":1}`)))
	require.NoError(t, st.SaveIdempotentResponse(ctx, "key_1", "idem_1", []byte(`{"n":2}`)))

	body, found, err := st.GetIdempotentResponse(ctx, "key_1", "idem_1")
	require.NoError(t, err)
	require.True(t, found)
	require.JSONEq(t, `{"n":1}`, string(body))
}
