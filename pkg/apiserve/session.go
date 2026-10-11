package apiserve

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/sipeed/picoclaw/pkg/session"
)

// ErrConversationBusy is returned when the same conversation already has an
// active turn and the server is configured to reject rather than queue.
var ErrConversationBusy = errors.New("apiserve: conversation busy")

// buildSessionScope is the single place the api channel's scope is defined.
//
// The conversation id MUST be part of the scope: without it two conversations of
// the same principal and agent hash to the same session key and silently share
// history. It is carried in the `chat` dimension, which is what that dimension
// means upstream; the value keeps the allocator's "<chatType>:<chatID>" shape
// (pkg/session/allocator.go) so chat-type inference still reads it correctly.
func buildSessionScope(agentID, account, sender, conversationID string) session.SessionScope {
	return session.SessionScope{
		Version:    session.ScopeVersionV1,
		AgentID:    agentID,
		Channel:    "api",
		Account:    account,
		Dimensions: []string{"chat", "sender"},
		Values: map[string]string{
			"chat":   "direct:" + conversationID,
			"sender": sender,
		},
	}
}

// identityFor derives (account, sender) from the principal.
//
// An empty user_id falls back to the account so the sender dimension is never
// dropped: pkg/session drops empty dimensions silently, which would merge every
// caller of one API key into a single shared session.
func identityFor(p *Principal, userID string) (account, sender string) {
	account = p.TenantID
	if account == "" {
		account = p.APIKeyID
	}
	sender = strings.TrimSpace(userID)
	if sender == "" {
		sender = account
	}
	return account, sender
}

// ensureConversation resolves (or creates) the conversation and its session key.
func (s *Server) ensureConversation(
	ctx context.Context,
	p *Principal,
	agentID, userID, conversationID string,
) (*Conversation, error) {
	account, sender := identityFor(p, userID)

	// Lower-case the caller-supplied id before it reaches either the row or the
	// scope. CanonicalScopeSignature trims and lower-cases scope values
	// (pkg/session/key.go:196) while conversation_id's UNIQUE index compares
	// bytes, so without this "CONV_A" and "conv_a" would hash to one session_key
	// but insert two rows — and the second insert would fail on session_key's
	// PRIMARY KEY, which ON CONFLICT(conversation_id) does not cover.
	convID := strings.ToLower(strings.TrimSpace(conversationID))
	if convID == "" {
		generated, err := newID("conv")
		if err != nil {
			return nil, err
		}
		convID = generated
	}

	scope := buildSessionScope(agentID, account, sender, convID)

	return s.store.EnsureConversation(ctx, EnsureConversationParams{
		APIKeyID:       p.APIKeyID,
		AgentID:        agentID,
		UserID:         sender,
		ConversationID: convID,
		SessionKey:     session.BuildSessionKey(scope),
	})
}

// Gate serializes turns within a conversation.
//
// The upstream ProcessDirect* path has no session guard: registerActiveTurn
// stores without checking (pkg/agent/turn_coord.go:31), and the only guarding
// entry points are AgentLoop.Run and AgentLoop.Continue. Without this gate two
// concurrent requests would run the same session in parallel and interleave
// writes to the same JSONL file.
type Gate struct {
	mu    sync.Mutex
	locks map[string]struct{}
}

// NewGate creates an empty conversation gate.
func NewGate() *Gate {
	return &Gate{locks: make(map[string]struct{})}
}

// TryAcquire claims the conversation. It returns ErrConversationBusy when the
// conversation is already running. The returned release function is safe to
// call more than once.
func (g *Gate) TryAcquire(conversationID string) (func(), error) {
	g.mu.Lock()
	if _, busy := g.locks[conversationID]; busy {
		g.mu.Unlock()
		return nil, ErrConversationBusy
	}
	g.locks[conversationID] = struct{}{}
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.locks, conversationID)
			g.mu.Unlock()
		})
	}, nil
}
