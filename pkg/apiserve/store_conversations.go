package apiserve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Conversation maps an opaque caller-visible id onto a server-issued session key.
type Conversation struct {
	ConversationID string
	SessionKey     string
	APIKeyID       string
	AgentID        string
	UserID         string
}

// EnsureConversationParams describes the conversation to create or reuse.
type EnsureConversationParams struct {
	APIKeyID       string
	AgentID        string
	UserID         string
	ConversationID string
	SessionKey     string
}

// EnsureConversation inserts the conversation on first use and returns the
// stored row afterwards. The stored session key always wins: a caller cannot
// rebind an existing conversation_id to another session or owner.
func (s *Store) EnsureConversation(
	ctx context.Context,
	p EnsureConversationParams,
) (*Conversation, error) {
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_index
			(session_key, conversation_id, api_key_id, agent_id, user_id, created_at, last_active_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conversation_id) DO NOTHING`,
		p.SessionKey, p.ConversationID, p.APIKeyID, p.AgentID, p.UserID, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert conversation: %w", err)
	}

	return s.ConversationFor(ctx, ConversationOwner{
		APIKeyID: p.APIKeyID,
		UserID:   p.UserID,
		AgentID:  p.AgentID,
	}, p.ConversationID)
}

// ConversationOwner identifies who a conversation belongs to.
//
// All three dimensions are part of the identity, and all three must match on
// every read. `AgentID` matters as much as the others: the agent is baked into
// the session key's scope, so a conversation created for one agent must not be
// handed to another — doing so would execute on agent B under agent A's session
// key (history written into B's workspace under A's key), which is exactly the
// silent ownership/capability mismatch spec 事实 13 warns about.
type ConversationOwner struct {
	APIKeyID string
	UserID   string
	AgentID  string
}

// ConversationFor returns a conversation belonging to owner.
//
// Every ownership dimension is enforced **in the query**, not by callers: any
// mismatch reports ErrNotFound instead of handing over another owner's session
// key. Keeping the rule here means no read path — including future ones — can
// skip it.
func (s *Store) ConversationFor(
	ctx context.Context,
	owner ConversationOwner,
	conversationID string,
) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT conversation_id, session_key, api_key_id, agent_id, user_id
		FROM session_index
		WHERE conversation_id = ? AND api_key_id = ? AND user_id = ? AND agent_id = ?`,
		conversationID, owner.APIKeyID, owner.UserID, owner.AgentID,
	)

	var conv Conversation
	err := row.Scan(&conv.ConversationID, &conv.SessionKey, &conv.APIKeyID, &conv.AgentID, &conv.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup conversation: %w", err)
	}

	_, err = s.db.ExecContext(
		ctx, `UPDATE session_index SET last_active_at = ? WHERE conversation_id = ?`,
		time.Now().Unix(), conversationID,
	)
	if err != nil {
		return nil, fmt.Errorf("touch conversation: %w", err)
	}

	return &conv, nil
}
