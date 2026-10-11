package apiserve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// idempotencyWindow bounds how long a stored response stays replayable
// (spec §4.6: "24h 内同 key 返回首次结果"). Without it a client that
// legitimately reuses a key later would get a stale replay, and the table would
// grow without bound.
const idempotencyWindow = 24 * time.Hour

// idempotencySeparator joins the API key id and the caller's key. It is the
// ASCII unit separator, which cannot appear in an HTTP header value, so the
// composite can never be ambiguous.
const idempotencySeparator = "\x1f"

// idempotencyScope namespaces a caller-supplied Idempotency-Key by API key.
//
// The header is the caller's own string, so keying the table on it alone would
// make it a global namespace: any key sending the same value would receive the
// other key's stored response body — for /v1/messages that is the agent's reply
// text, the session key and the conversation id.
func idempotencyScope(apiKeyID, key string) string {
	return apiKeyID + idempotencySeparator + key
}

// GetIdempotentResponse returns a previously stored response body, if the record
// is still inside the replay window and belongs to this API key.
func (s *Store) GetIdempotentResponse(
	ctx context.Context,
	apiKeyID, key string,
) ([]byte, bool, error) {
	if key == "" {
		return nil, false, nil
	}

	var body string
	err := s.db.QueryRowContext(ctx,
		`SELECT response FROM idempotency WHERE key = ? AND created_at > ?`,
		idempotencyScope(apiKeyID, key), time.Now().Add(-idempotencyWindow).Unix(),
	).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load idempotent response: %w", err)
	}
	return []byte(body), true, nil
}

// SaveIdempotentResponse stores the first response for a key, scoped to the API
// key that produced it. Later writes for the same (api key, key) pair are
// ignored so a replay always returns the original body.
//
// Expired rows are pruned opportunistically on the way through, which keeps the
// table bounded without a separate sweeper. A prune failure is logged rather
// than returned: it must not fail a request whose response was already sent.
func (s *Store) SaveIdempotentResponse(ctx context.Context, apiKeyID, key string, body []byte) error {
	if key == "" {
		return nil
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency WHERE created_at <= ?`,
		time.Now().Add(-idempotencyWindow).Unix(),
	); err != nil {
		logger.WarnCF("apiserve", "failed to prune expired idempotency rows",
			map[string]any{"error": err.Error()})
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotency (key, response, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO NOTHING`,
		idempotencyScope(apiKeyID, key), string(body), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("save idempotent response: %w", err)
	}
	return nil
}
