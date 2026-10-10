package apiserve

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// apiKeyPrefix marks generated keys.
const apiKeyPrefix = "pk_live_"

// APIKey is a stored credential. KeyHash is the only secret-derived value kept.
type APIKey struct {
	ID            string
	KeyHash       string
	Name          string
	OwnerType     string
	TenantID      string
	AllowedAgents []string
	AllowedSkills []string
	RateLimit     string
	Enabled       bool
}

// CreateAPIKeyParams describes a key to create. Nil slices mean "unrestricted".
type CreateAPIKeyParams struct {
	Name          string
	OwnerType     string
	TenantID      string
	AllowedAgents []string
	AllowedSkills []string
	RateLimit     string
}

// NewAPIKeyPlaintext generates a new random API key.
func NewAPIKeyPlaintext() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return apiKeyPrefix + hex.EncodeToString(buf), nil
}

// HashAPIKey returns the hex sha256 of a plaintext key.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func encodeStringList(values []string) string {
	if len(values) == 0 {
		return ""
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeStringList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// CreateAPIKey stores a new key and returns it together with its plaintext.
// The plaintext is returned exactly once and never persisted.
func (s *Store) CreateAPIKey(
	ctx context.Context,
	p CreateAPIKeyParams,
) (APIKey, string, error) {
	plaintext, err := NewAPIKeyPlaintext()
	if err != nil {
		return APIKey{}, "", err
	}

	id, err := newID("key")
	if err != nil {
		return APIKey{}, "", err
	}

	key := APIKey{
		ID:            id,
		KeyHash:       HashAPIKey(plaintext),
		Name:          p.Name,
		OwnerType:     p.OwnerType,
		TenantID:      p.TenantID,
		AllowedAgents: p.AllowedAgents,
		AllowedSkills: p.AllowedSkills,
		RateLimit:     p.RateLimit,
		Enabled:       true,
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO api_keys
			(id, key_hash, name, owner_type, tenant_id, allowed_agents, allowed_skills,
			 rate_limit, enabled, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, 0)`,
		key.ID, key.KeyHash, key.Name, key.OwnerType, key.TenantID,
		encodeStringList(key.AllowedAgents), encodeStringList(key.AllowedSkills),
		key.RateLimit, time.Now().Unix(),
	)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("insert api key: %w", err)
	}

	return key, plaintext, nil
}

// LookupAPIKey resolves a plaintext key. Unknown keys yield ErrNotFound and
// disabled keys yield ErrKeyDisabled so callers can answer 401 uniformly.
func (s *Store) LookupAPIKey(ctx context.Context, plaintext string) (*APIKey, error) {
	if strings.TrimSpace(plaintext) == "" {
		return nil, ErrNotFound
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT id, key_hash, name, owner_type, tenant_id, allowed_agents, allowed_skills,
		       rate_limit, enabled
		FROM api_keys WHERE key_hash = ?`, HashAPIKey(plaintext))

	var (
		key       APIKey
		agentsRaw string
		skillsRaw string
		enabled   int
	)
	err := row.Scan(
		&key.ID, &key.KeyHash, &key.Name, &key.OwnerType, &key.TenantID,
		&agentsRaw, &skillsRaw, &key.RateLimit, &enabled,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup api key: %w", err)
	}

	key.AllowedAgents = decodeStringList(agentsRaw)
	key.AllowedSkills = decodeStringList(skillsRaw)
	key.Enabled = enabled != 0
	if !key.Enabled {
		return nil, ErrKeyDisabled
	}
	return &key, nil
}

// TouchAPIKey records the last successful use of a key.
func (s *Store) TouchAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(
		ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("touch api key: %w", err)
	}
	return nil
}

// APIKeyHashByID returns the sha256 hash of a key, which doubles as the HMAC
// key for outbound callback signatures.
func (s *Store) APIKeyHashByID(ctx context.Context, id string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT key_hash FROM api_keys WHERE id = ?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup api key hash: %w", err)
	}
	return hash, nil
}

// ListAPIKeys returns every key, newest first.
//
// KeyHash is populated because the caller is in-process, but no code path may
// ever render it (see Task 13's "list 不得输出明文或 hash" 断言).
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, key_hash, name, owner_type, tenant_id, allowed_agents, allowed_skills,
		       rate_limit, enabled
		FROM api_keys ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []APIKey
	for rows.Next() {
		var (
			key       APIKey
			agentsRaw string
			skillsRaw string
			enabled   int
		)
		if err := rows.Scan(
			&key.ID, &key.KeyHash, &key.Name, &key.OwnerType, &key.TenantID,
			&agentsRaw, &skillsRaw, &key.RateLimit, &enabled,
		); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		key.AllowedAgents = decodeStringList(agentsRaw)
		key.AllowedSkills = decodeStringList(skillsRaw)
		key.Enabled = enabled != 0
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api keys: %w", err)
	}
	return out, nil
}
