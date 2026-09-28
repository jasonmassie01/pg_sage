package agentdb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func NewStore(pool *pgxpool.Pool) *Store {
	return NewStoreWithOptions(pool, DefaultStoreOptions())
}

// agentDBSchemaVersion is bumped whenever schemaStatements change.
const agentDBSchemaVersion = 2026092701

// schemaReady memoizes successful initialization per pool so request paths
// (including the unauthenticated agent-ping) never re-run DDL (G8-B16).
var schemaReady sync.Map

// Ensure initializes the AgentDB schema once per pool per process. A
// database already at agentDBSchemaVersion skips all DDL (ALTER TABLE ... ADD
// COLUMN IF NOT EXISTS takes ACCESS EXCLUSIVE locks) and re-seeding.
func (s *Store) Ensure(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("agentdb store unavailable")
	}
	if _, ok := schemaReady.Load(s.pool); ok {
		return nil
	}
	if s.schemaCurrent(ctx) {
		schemaReady.Store(s.pool, struct{}{})
		return nil
	}
	if err := s.initializeSchema(ctx); err != nil {
		return err
	}
	schemaReady.Store(s.pool, struct{}{})
	return nil
}

func (s *Store) schemaCurrent(ctx context.Context) bool {
	var version int
	err := s.pool.QueryRow(ctx, `/* pg_sage */
		SELECT version FROM sage.agent_db_schema_version WHERE singleton`).Scan(&version)
	return err == nil && version == agentDBSchemaVersion
}

func (s *Store) initializeSchema(ctx context.Context) error {
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
	if _, err := tx.Exec(ctx, `INSERT INTO sage.agent_db_schema_version (singleton, version)
		VALUES (true, $1)
		ON CONFLICT (singleton) DO UPDATE SET version=EXCLUDED.version, updated_at=now()`,
		agentDBSchemaVersion); err != nil {
		return fmt.Errorf("record agentdb schema version: %w", err)
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
