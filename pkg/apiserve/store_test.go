package apiserve

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenStore_CreatesAllTables(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	for _, table := range []string{
		"api_keys",
		"session_index",
		"usage_records",
		"tasks",
		"idempotency",
	} {
		var name string
		err := st.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		require.NoErrorf(t, err, "table %q must exist", table)
		require.Equal(t, table, name)
	}
}

func TestOpenStore_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	first, err := OpenStore(path)
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second, err := OpenStore(path)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}

// 全新安装时 home 目录可能还不存在：OpenStore 必须自己建出父目录，
// 否则 picoclaw-api 在干净机器上第一次启动就会失败。
func TestOpenStore_CreatesMissingParentDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "fresh", "home")
	require.NoDirExists(t, home)

	st, err := OpenStore(filepath.Join(home, "apiserve.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	require.DirExists(t, home)
}

// WAL 与 busy_timeout 是 Task 13 的硬前提（CLI 会开第二个句柄写同一个文件），
// 所以这里直接断言 pragma 生效，而不是靠并发时序去碰运气。
func TestOpenStore_EnablesWALAndBusyTimeout(t *testing.T) {
	st := newTestStore(t)

	var journalMode string
	require.NoError(t, st.db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode))
	require.Equal(t, "wal", strings.ToLower(journalMode))

	var busyTimeout int
	require.NoError(t, st.db.QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeout))
	require.Equal(t, 5000, busyTimeout)

	var foreignKeys int
	require.NoError(t, st.db.QueryRow("PRAGMA foreign_keys;").Scan(&foreignKeys))
	require.Equal(t, 1, foreignKeys)
}

// 回归护栏：两个句柄并发写同一个库不得报 "database is locked"。
// （时序相关，不保证在缺少 pragma 时必挂；上面那条 pragma 断言才是确定性的证据。）
func TestOpenStore_ConcurrentWritersFromTwoHandlesSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	first, err := OpenStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })

	second, err := OpenStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

	const perHandle = 10
	errCh := make(chan error, perHandle*2)

	var wg sync.WaitGroup
	for i := 0; i < perHandle; i++ {
		for _, handle := range []*Store{first, second} {
			wg.Add(1)
			go func(st *Store, n int) {
				defer wg.Done()
				if _, _, err := st.CreateAPIKey(context.Background(), CreateAPIKeyParams{
					Name:      fmt.Sprintf("svc-%d", n),
					OwnerType: "internal_service",
				}); err != nil {
					errCh <- err
				}
			}(handle, i)
		}
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	keys, err := first.ListAPIKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, keys, perHandle*2)
}
