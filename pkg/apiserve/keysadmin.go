package apiserve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetAPIKeyByID loads a key by id, including disabled ones.
func (s *Store) GetAPIKeyByID(ctx context.Context, id string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, key_hash, name, owner_type, tenant_id, allowed_agents, allowed_skills,
		       rate_limit, enabled
		FROM api_keys WHERE id = ?`, id)

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
		return nil, fmt.Errorf("get api key: %w", err)
	}

	key.AllowedAgents = decodeStringList(agentsRaw)
	key.AllowedSkills = decodeStringList(skillsRaw)
	key.Enabled = enabled != 0
	return &key, nil
}

// SetAPIKeyEnabled enables or disables a key. Unknown ids yield ErrNotFound so
// the CLI cannot report success for a typo.
func (s *Store) SetAPIKeyEnabled(ctx context.Context, id string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}

	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET enabled = ? WHERE id = ?`, value, id)
	if err != nil {
		return fmt.Errorf("set api key enabled: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set api key enabled: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateAPIKey creates a replacement for key id and disables the original.
// The new plaintext is returned exactly once and never persisted.
//
// Ordering is create-then-disable on purpose: if the disable step fails the
// operator is left with two live keys (safe) instead of zero (an outage). When
// that happens the returned error says the replacement already exists, so the
// operator disables the old key by hand instead of blindly retrying.
func (s *Store) RotateAPIKey(ctx context.Context, id string) (APIKey, string, error) {
	original, err := s.GetAPIKeyByID(ctx, id)
	if err != nil {
		return APIKey{}, "", err
	}

	replacement, plaintext, err := s.CreateAPIKey(ctx, CreateAPIKeyParams{
		Name:          original.Name,
		OwnerType:     original.OwnerType,
		TenantID:      original.TenantID,
		AllowedAgents: original.AllowedAgents,
		AllowedSkills: original.AllowedSkills,
		RateLimit:     original.RateLimit,
	})
	if err != nil {
		return APIKey{}, "", err
	}

	if err := s.SetAPIKeyEnabled(ctx, id, false); err != nil {
		return replacement, plaintext, fmt.Errorf(
			"replacement %s created but disabling %s failed: %w", replacement.ID, id, err)
	}

	return replacement, plaintext, nil
}
