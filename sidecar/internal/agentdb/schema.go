package agentdb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ensure(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("agentdb store unavailable")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin agentdb schema initialization: %w", err)
	}
	defer rollbackSchemaInit(tx)
	// One database-wide lock covers all Store instances and processes. READ COMMITTED
	// gives waiters a fresh catalog snapshot after the previous initializer commits.
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock($1)",
		int64(0x5047534741474442)); err != nil {
		return fmt.Errorf("lock agentdb schema initialization: %w", err)
	}
	for _, stmt := range schemaStatements {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("initialize agentdb schema: %w", err)
		}
	}
	if err := seedDefaultSizeProfiles(ctx, tx); err != nil {
		return fmt.Errorf("seed agentdb default profiles: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agentdb schema initialization: %w", err)
	}
	return nil
}

func rollbackSchemaInit(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A rollback failure makes pgx destroy the connection, releasing transaction locks.
	_ = tx.Rollback(ctx)
}

func (s *Store) ProvisionSchema(
	ctx context.Context,
	req RegisterRequest,
) (Deployment, error) {
	normalizeProviderFields(&req)
	if req.Provider != ProviderLocalPostgres || req.ProvisioningLevel != LevelSchema {
		return Deployment{}, ErrInvalid
	}
	req.SchemaName = sanitizeSchemaName(req.SchemaName)
	if req.SchemaName == "" {
		req.SchemaName = "agentdb_" + idFrom(req.TenantID, req.AgentID)
	}
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	sql := fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdent(req.SchemaName))
	if _, err := s.pool.Exec(ctx, sql); err != nil {
		return Deployment{}, err
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	req.Metadata["credential_scope"] = req.SchemaName
	req.ProvisioningStatus = "provisioned"
	req.ConnectionInfo = map[string]any{
		"provider":    req.Provider,
		"schema_name": req.SchemaName,
	}
	return s.Register(ctx, req)
}

func (s *Store) Provision(ctx context.Context, req RegisterRequest) (Deployment, error) {
	normalizeProviderFields(&req)
	if req.Provider == ProviderLocalPostgres {
		switch req.ProvisioningLevel {
		case LevelSchema:
			return s.ProvisionSchema(ctx, req)
		case LevelDatabase:
			return s.provisionLocalDatabase(ctx, req)
		default:
			return Deployment{}, ErrInvalid
		}
	}
	if req.ProvisioningLevel != LevelInstance {
		return Deployment{}, ErrInvalid
	}
	profile, err := s.profileForRequest(ctx, req)
	if err != nil {
		return Deployment{}, err
	}
	profile = normalizedProvisionProfile(profile)
	plan, err := BuildProvisionPlan(req, profile)
	if err != nil {
		return Deployment{}, err
	}
	req.ProvisioningStatus = "planned"
	req.ProvisioningPlan = planMap(plan)
	req.SizeProfileID = profile.ProfileID
	req.Metadata = cloneAnyMap(req.Metadata)
	req.Metadata["provider_params"] = cloneAnyMap(profile.ProviderParams)
	req.Metadata["size_profile_id"] = profile.ProfileID
	return s.Register(ctx, req)
}

func (s *Store) provisionLocalDatabase(
	ctx context.Context,
	req RegisterRequest,
) (Deployment, error) {
	req.DatabaseName = sanitizeDatabaseName(req.DatabaseName)
	if req.DatabaseName == "" {
		req.DatabaseName = "agentdb_" + idFrom(req.TenantID, req.AgentID)
	}
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	sql := fmt.Sprintf("CREATE DATABASE %s", quoteIdent(req.DatabaseName))
	if _, err := s.pool.Exec(ctx, sql); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return Deployment{}, err
		}
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	req.Metadata["credential_scope"] = req.DatabaseName
	req.ProvisioningStatus = "provisioned"
	req.ConnectionInfo = map[string]any{
		"provider":      req.Provider,
		"database_name": req.DatabaseName,
	}
	return s.Register(ctx, req)
}

func jsonBytes(v map[string]any) []byte {
	if v == nil {
		return []byte(`{}`)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func jsonAny(v any) []byte {
	if v == nil {
		return []byte(`null`)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`null`)
	}
	return b
}

func sanitizeSchemaName(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range v {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r)
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "agentdb_" + out
	}
	if len(out) > 63 {
		return out[:63]
	}
	return out
}

func sanitizeDatabaseName(v string) string {
	return sanitizeSchemaName(v)
}

func quoteIdent(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}
