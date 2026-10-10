package apiserve

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("apiserve: not found")

// ErrKeyDisabled is returned when an API key exists but is disabled.
var ErrKeyDisabled = errors.New("apiserve: api key disabled")

// schemaSQL is applied on every OpenStore; all statements are idempotent.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS api_keys (
  id             TEXT PRIMARY KEY,
  key_hash       TEXT NOT NULL UNIQUE,
  name           TEXT NOT NULL,
  owner_type     TEXT NOT NULL,
  tenant_id      TEXT NOT NULL DEFAULT '',
  allowed_agents TEXT NOT NULL DEFAULT '',
  allowed_skills TEXT NOT NULL DEFAULT '',
  rate_limit     TEXT NOT NULL DEFAULT '',
  enabled        INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  last_used_at   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS session_index (
  session_key     TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL UNIQUE,
  api_key_id      TEXT NOT NULL,
  agent_id        TEXT NOT NULL,
  user_id         TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  last_active_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_session_index_owner ON session_index(api_key_id);

CREATE TABLE IF NOT EXISTS usage_records (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  api_key_id        TEXT NOT NULL,
  agent_id          TEXT NOT NULL,
  user_id           TEXT NOT NULL DEFAULT '',
  conversation_id   TEXT NOT NULL DEFAULT '',
  session_key       TEXT NOT NULL DEFAULT '',
  request_id        TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_agent ON usage_records(agent_id, created_at);

CREATE TABLE IF NOT EXISTS tasks (
  id                TEXT PRIMARY KEY,
  status            TEXT NOT NULL,
  api_key_id        TEXT NOT NULL,
  agent_id          TEXT NOT NULL,
  user_id           TEXT NOT NULL DEFAULT '',
  conversation_id   TEXT NOT NULL,
  session_key       TEXT NOT NULL,
  request_json      TEXT NOT NULL,
  result_json       TEXT NOT NULL DEFAULT '',
  error_code        TEXT NOT NULL DEFAULT '',
  error_message     TEXT NOT NULL DEFAULT '',
  callback_url      TEXT NOT NULL DEFAULT '',
  callback_status   TEXT NOT NULL DEFAULT '',
  callback_attempts INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  started_at        INTEGER NOT NULL DEFAULT 0,
  finished_at       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, created_at);

CREATE TABLE IF NOT EXISTS idempotency (
  key        TEXT PRIMARY KEY,
  response   TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
`

// Store is the SQLite-backed persistence layer of the API service.
type Store struct {
	db *sql.DB
}

// OpenStore opens (and migrates) the SQLite database at path.
//
// Pragmas follow the existing repo pattern (pkg/seahorse/short_engine.go:109-121,
// pkg/channels/matrix/matrix.go:335-350). WAL + busy_timeout are required, not
// optional: Task 13's `picoclaw-api keys` CLI opens a *second* handle on the same
// file while the server is running, and without them concurrent writers fail
// with SQLITE_BUSY.
func OpenStore(path string) (*Store, error) {
	// A fresh machine has no ~/.picoclaw yet, and neither this service nor its
	// CLI creates it elsewhere: SQLite cannot create a database file inside a
	// missing directory, so the first `picoclaw-api` start would fail.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory %q: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite writers in this process must be serialized.
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, stmt := range pragmas {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply %q: %w", stmt, err)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// newID returns a short random identifier with the given prefix.
func newID(prefix string) (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(buf), nil
}
