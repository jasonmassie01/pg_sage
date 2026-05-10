package agentdb

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (s *Store) UpsertProviderConfig(
	ctx context.Context,
	req ProviderConfigRequest,
) (ProviderConfig, error) {
	if err := s.Ensure(ctx); err != nil {
		return ProviderConfig{}, err
	}
	req.Provider = normalizeProvider(req.Provider)
	if !validProvider(req.Provider) || req.Provider == ProviderLocalPostgres {
		return ProviderConfig{}, ErrInvalid
	}
	if req.Settings == nil {
		req.Settings = map[string]any{}
	}
	if err := rejectSecretSettings(req.Settings); err != nil {
		return ProviderConfig{}, err
	}
	var cfg ProviderConfig
	err := scanProviderConfig(s.pool.QueryRow(ctx, `
		INSERT INTO sage.agent_db_provider_configs (provider, enabled, settings)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (provider) DO UPDATE
		SET enabled=EXCLUDED.enabled,
			settings=EXCLUDED.settings,
			updated_at=now()
		RETURNING provider, enabled, settings, last_validated_at,
			created_at, updated_at`,
		req.Provider, req.Enabled, jsonBytes(req.Settings),
	), &cfg)
	return cfg, err
}

func (s *Store) ProviderConfig(ctx context.Context, provider string) (ProviderConfig, error) {
	if err := s.Ensure(ctx); err != nil {
		return ProviderConfig{}, err
	}
	provider = normalizeProvider(provider)
	var cfg ProviderConfig
	err := scanProviderConfig(s.pool.QueryRow(ctx, `
		SELECT provider, enabled, settings, last_validated_at, created_at, updated_at
		FROM sage.agent_db_provider_configs
		WHERE provider=$1`, provider), &cfg)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderConfig{}, ErrNotFound
	}
	cfg.Settings = RedactProviderDetail(cfg.Settings)
	return cfg, err
}

func (s *Store) ProviderConfigs(ctx context.Context) ([]ProviderConfig, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT provider, enabled, settings, last_validated_at, created_at, updated_at
		FROM sage.agent_db_provider_configs
		ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderConfig{}
	for rows.Next() {
		var cfg ProviderConfig
		if err := scanProviderConfig(rows, &cfg); err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, rows.Err()
}

func scanProviderConfig(row scanner, cfg *ProviderConfig) error {
	err := row.Scan(
		&cfg.Provider,
		&cfg.Enabled,
		&cfg.Settings,
		&cfg.LastValidated,
		&cfg.CreatedAt,
		&cfg.UpdatedAt,
	)
	cfg.Settings = RedactProviderDetail(cfg.Settings)
	return err
}
