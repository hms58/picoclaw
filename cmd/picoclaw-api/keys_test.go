package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/apiserve"
)

// runKeys executes the keys command group in-process and returns its output.
func runKeys(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newKeysCommand()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// extractValue reads the value after "<label>" on the first matching line.
func extractValue(t *testing.T, out, label string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if idx := strings.Index(line, label); idx >= 0 {
			return strings.TrimSpace(line[idx+len(label):])
		}
	}
	t.Fatalf("label %q not found in output:\n%s", label, out)
	return ""
}

func TestKeysCLI_ListNeverLeaksPlaintextOrHash(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apiserve.db")

	out, err := runKeys(t, "create", "--db", dbPath, "--name", "order-service",
		"--allowed-agents", "main,stock", "--rate-limit", "20/s,8")
	require.NoError(t, err)

	plaintext := extractValue(t, out, "api_key:")
	keyID := extractValue(t, out, "id:")
	require.True(t, strings.HasPrefix(plaintext, "pk_live_"))

	for _, args := range [][]string{
		{"list", "--db", dbPath},
		{"list", "--db", dbPath, "--json"},
	} {
		out, err = runKeys(t, args...)
		require.NoError(t, err)
		require.Contains(t, out, "order-service")
		require.NotContains(t, out, plaintext, "明文绝不能出现在 list 输出里")
		require.NotContains(t, out, apiserve.HashAPIKey(plaintext), "hash 也绝不能出现")
		require.NotContains(t, out, "key_hash")
	}

	// --json 的输出必须能被解析，且每个对象都没有 hash 字段
	out, err = runKeys(t, "list", "--db", dbPath, "--json")
	require.NoError(t, err)

	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, keyID, rows[0]["id"])
	require.Equal(t, "order-service", rows[0]["name"])
	_, hasHash := rows[0]["key_hash"]
	require.False(t, hasHash)
}

func TestKeysCLI_DisableThenEnable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apiserve.db")

	out, err := runKeys(t, "create", "--db", dbPath, "--name", "svc")
	require.NoError(t, err)
	keyID := extractValue(t, out, "id:")
	plaintext := extractValue(t, out, "api_key:")

	store, err := apiserve.OpenStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	_, err = runKeys(t, "disable", "--db", dbPath, "--id", keyID)
	require.NoError(t, err)
	_, err = store.LookupAPIKey(context.Background(), plaintext)
	require.ErrorIs(t, err, apiserve.ErrKeyDisabled)

	_, err = runKeys(t, "enable", "--db", dbPath, "--id", keyID)
	require.NoError(t, err)
	_, err = store.LookupAPIKey(context.Background(), plaintext)
	require.NoError(t, err)
}

func TestKeysCLI_RotateReplacesAndDisables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apiserve.db")

	out, err := runKeys(t, "create", "--db", dbPath, "--name", "order-service")
	require.NoError(t, err)
	oldID := extractValue(t, out, "id:")
	oldPlaintext := extractValue(t, out, "api_key:")

	out, err = runKeys(t, "rotate", "--db", dbPath, "--id", oldID)
	require.NoError(t, err)
	newPlaintext := extractValue(t, out, "api_key:")
	require.NotEqual(t, oldPlaintext, newPlaintext)
	require.Contains(t, out, "old_id:   "+oldID)

	store, err := apiserve.OpenStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	_, err = store.LookupAPIKey(context.Background(), oldPlaintext)
	require.ErrorIs(t, err, apiserve.ErrKeyDisabled)

	lookedUp, err := store.LookupAPIKey(context.Background(), newPlaintext)
	require.NoError(t, err)
	require.Equal(t, "order-service", lookedUp.Name)
}
