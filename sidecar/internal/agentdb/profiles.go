package agentdb

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Store) UpsertSizeProfile(
	ctx context.Context,
	profile SizeProfile,
) (SizeProfile, error) {
	if err := s.Ensure(ctx); err != nil {
		return SizeProfile{}, err
	}
	if err := normalizeProfile(&profile); err != nil {
		return SizeProfile{}, err
	}
	var out SizeProfile
	err := scanSizeProfile(s.pool.QueryRow(ctx, `
		INSERT INTO sage.agent_db_size_profiles (
			profile_id, provider, provisioning_level, name, description, cpu,
			memory_gb, storage_gb, max_connections, monthly_budget_usd,
			provider_params
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)
		ON CONFLICT (profile_id) DO UPDATE
		SET provider=EXCLUDED.provider,
			provisioning_level=EXCLUDED.provisioning_level,
			name=EXCLUDED.name,
			description=EXCLUDED.description,
			cpu=EXCLUDED.cpu,
			memory_gb=EXCLUDED.memory_gb,
			storage_gb=EXCLUDED.storage_gb,
			max_connections=EXCLUDED.max_connections,
			monthly_budget_usd=EXCLUDED.monthly_budget_usd,
			provider_params=EXCLUDED.provider_params,
			updated_at=now()
		RETURNING profile_id, provider, provisioning_level, name, description,
			cpu, memory_gb, storage_gb, max_connections, monthly_budget_usd,
			provider_params, created_at, updated_at`,
		profile.ProfileID,
		profile.Provider,
		profile.ProvisioningLevel,
		profile.Name,
		profile.Description,
		profile.CPU,
		profile.MemoryGB,
		profile.StorageGB,
		profile.MaxConnections,
		profile.MonthlyBudgetUSD,
		jsonBytes(profile.ProviderParams),
	), &out)
	return out, err
}

func (s *Store) ListSizeProfiles(ctx context.Context) ([]SizeProfile, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT profile_id, provider, provisioning_level, name, description,
			cpu, memory_gb, storage_gb, max_connections, monthly_budget_usd,
			provider_params, created_at, updated_at
		FROM sage.agent_db_size_profiles
		ORDER BY provider, provisioning_level, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SizeProfile{}
	for rows.Next() {
		var profile SizeProfile
		if err := scanSizeProfile(rows, &profile); err != nil {
			return nil, err
		}
		out = append(out, profile)
	}
	return out, rows.Err()
}

func (s *Store) GetSizeProfile(ctx context.Context, id string) (SizeProfile, error) {
	if err := s.Ensure(ctx); err != nil {
		return SizeProfile{}, err
	}
	var profile SizeProfile
	err := scanSizeProfile(s.pool.QueryRow(ctx, `
		SELECT profile_id, provider, provisioning_level, name, description,
			cpu, memory_gb, storage_gb, max_connections, monthly_budget_usd,
			provider_params, created_at, updated_at
		FROM sage.agent_db_size_profiles
		WHERE profile_id=$1`, id,
	), &profile)
	if errors.Is(err, pgx.ErrNoRows) {
		return SizeProfile{}, ErrNotFound
	}
	return profile, err
}

func (s *Store) DeleteSizeProfile(ctx context.Context, id string) error {
	if err := s.Ensure(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM sage.agent_db_size_profiles WHERE profile_id=$1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) profileForRequest(
	ctx context.Context,
	req RegisterRequest,
) (SizeProfile, error) {
	if req.SizeProfileID != "" {
		return s.GetSizeProfile(ctx, req.SizeProfileID)
	}
	profiles, err := s.ListSizeProfiles(ctx)
	if err != nil {
		return SizeProfile{}, err
	}
	for _, profile := range profiles {
		if profile.Provider == req.Provider &&
			profile.ProvisioningLevel == req.ProvisioningLevel {
			return profile, nil
		}
	}
	return SizeProfile{}, ErrNotFound
}

func (s *Store) seedDefaultSizeProfiles(ctx context.Context) error {
	for _, profile := range defaultSizeProfiles() {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO sage.agent_db_size_profiles (
				profile_id, provider, provisioning_level, name, description,
				cpu, memory_gb, storage_gb, max_connections,
				monthly_budget_usd, provider_params
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)
			ON CONFLICT (profile_id) DO NOTHING`,
			profile.ProfileID,
			profile.Provider,
			profile.ProvisioningLevel,
			profile.Name,
			profile.Description,
			profile.CPU,
			profile.MemoryGB,
			profile.StorageGB,
			profile.MaxConnections,
			profile.MonthlyBudgetUSD,
			jsonBytes(profile.ProviderParams),
		); err != nil {
			return err
		}
	}
	return nil
}

func defaultSizeProfiles() []SizeProfile {
	return []SizeProfile{
		localProfile("local_schema_xs", LevelSchema, "Local schema XS"),
		localProfile("local_database_s", LevelDatabase, "Local database S"),
		cloudProfile("rds_instance_s", ProviderAWSRDS, "RDS instance S",
			map[string]any{"db_instance_class": "db.t4g.micro"}),
		cloudProfile("cloudsql_instance_s", ProviderGCPCloudSQL, "Cloud SQL instance S",
			map[string]any{
				"tier":             "db-custom-1-3840",
				"database_version": "POSTGRES_16",
				"edition":          "ENTERPRISE",
				"ipv4_enabled":     true,
				"require_ssl":      true,
			}),
		cloudProfile("lakebase_instance_s", ProviderDatabricksLakebase,
			"Lakebase instance S", map[string]any{"mode": "autoscaling_branch"}),
	}
}

func localProfile(id, level, name string) SizeProfile {
	return SizeProfile{
		ProfileID:         id,
		Provider:          ProviderLocalPostgres,
		ProvisioningLevel: level,
		Name:              name,
		Description:       "Default local Postgres profile",
		CPU:               1,
		MemoryGB:          1,
		StorageGB:         5,
		MaxConnections:    20,
		MonthlyBudgetUSD:  0,
		ProviderParams:    map[string]any{},
	}
}

func cloudProfile(id, provider, name string, params map[string]any) SizeProfile {
	return SizeProfile{
		ProfileID:         id,
		Provider:          provider,
		ProvisioningLevel: LevelInstance,
		Name:              name,
		Description:       "Default cloud instance profile",
		CPU:               1,
		MemoryGB:          4,
		StorageGB:         20,
		MaxConnections:    100,
		MonthlyBudgetUSD:  75,
		ProviderParams:    params,
	}
}

func normalizeProfile(profile *SizeProfile) error {
	profile.ProfileID = strings.TrimSpace(profile.ProfileID)
	profile.Provider = normalizeProvider(profile.Provider)
	profile.ProvisioningLevel = normalizeProvisioningLevel(profile.ProvisioningLevel)
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.ProfileID == "" || profile.Name == "" {
		return ErrInvalid
	}
	if !validProvider(profile.Provider) || !validLevel(profile.ProvisioningLevel) {
		return ErrInvalid
	}
	if cloudProvider(profile.Provider) && profile.ProvisioningLevel != LevelInstance {
		return ErrInvalid
	}
	if profile.ProviderParams == nil {
		profile.ProviderParams = map[string]any{}
	}
	return nil
}

func scanSizeProfile(row scanner, profile *SizeProfile) error {
	return row.Scan(
		&profile.ProfileID,
		&profile.Provider,
		&profile.ProvisioningLevel,
		&profile.Name,
		&profile.Description,
		&profile.CPU,
		&profile.MemoryGB,
		&profile.StorageGB,
		&profile.MaxConnections,
		&profile.MonthlyBudgetUSD,
		&profile.ProviderParams,
		&profile.CreatedAt,
		&profile.UpdatedAt,
	)
}
