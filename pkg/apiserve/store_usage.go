package apiserve

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// UsageQuery filters and groups usage aggregation.
type UsageQuery struct {
	APIKeyID string
	AgentID  string
	From     time.Time
	To       time.Time
	GroupBy  string // "agent" | "api_key" | "user" | "" (total)
}

// UsageRow is one aggregation bucket.
type UsageRow struct {
	Key              string
	Requests         int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// RecordUsage appends one usage record.
func (s *Store) RecordUsage(ctx context.Context, rec UsageRecord) error {
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_records
			(api_key_id, agent_id, user_id, conversation_id, session_key, request_id, model,
			 prompt_tokens, completion_tokens, total_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.APIKeyID, rec.AgentID, rec.UserID, rec.ConversationID, rec.SessionKey,
		rec.RequestID, rec.Model, rec.PromptTokens, rec.CompletionTokens,
		rec.TotalTokens, createdAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}
	return nil
}

// SumUsageByRequest aggregates the usage rows of a single request id, restricted
// to one API key. A turn may call the LLM more than once (tool loops), so this
// must sum, not fetch.
//
// The key filter is not optional: request_id comes from the caller-supplied
// X-Request-Id header, so filtering on it alone would let two keys sharing that
// header see each other's token counts in their response bodies.
func (s *Store) SumUsageByRequest(
	ctx context.Context,
	apiKeyID, requestID string,
) (UsageRow, error) {
	if strings.TrimSpace(requestID) == "" {
		return UsageRow{}, nil
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(total_tokens), 0), COUNT(*)
		FROM usage_records WHERE request_id = ? AND api_key_id = ?`,
		requestID, apiKeyID)

	var out UsageRow
	if err := row.Scan(
		&out.PromptTokens, &out.CompletionTokens, &out.TotalTokens, &out.Requests,
	); err != nil {
		return UsageRow{}, fmt.Errorf("sum usage by request: %w", err)
	}
	return out, nil
}

// SumUsage aggregates usage records matching the query.
func (s *Store) SumUsage(ctx context.Context, q UsageQuery) ([]UsageRow, error) {
	groupColumn := ""
	switch strings.ToLower(strings.TrimSpace(q.GroupBy)) {
	case "agent":
		groupColumn = "agent_id"
	case "api_key":
		groupColumn = "api_key_id"
	case "user":
		groupColumn = "user_id"
	case "":
		groupColumn = ""
	default:
		return nil, fmt.Errorf("unsupported group_by %q", q.GroupBy)
	}

	selectExpr := "'' AS bucket"
	if groupColumn != "" {
		selectExpr = groupColumn + " AS bucket"
	}

	query := `SELECT ` + selectExpr + `,
			COUNT(*), COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(total_tokens), 0)
		FROM usage_records WHERE 1 = 1`
	args := make([]any, 0, 4)

	if q.APIKeyID != "" {
		query += " AND api_key_id = ?"
		args = append(args, q.APIKeyID)
	}
	if q.AgentID != "" {
		query += " AND agent_id = ?"
		args = append(args, q.AgentID)
	}
	if !q.From.IsZero() {
		query += " AND created_at >= ?"
		args = append(args, q.From.Unix())
	}
	if !q.To.IsZero() {
		query += " AND created_at <= ?"
		args = append(args, q.To.Unix())
	}
	if groupColumn != "" {
		query += " GROUP BY " + groupColumn + " ORDER BY " + groupColumn
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageRow
	for rows.Next() {
		var row UsageRow
		if err := rows.Scan(
			&row.Key, &row.Requests, &row.PromptTokens,
			&row.CompletionTokens, &row.TotalTokens,
		); err != nil {
			return nil, fmt.Errorf("scan usage row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage rows: %w", err)
	}
	return out, nil
}
