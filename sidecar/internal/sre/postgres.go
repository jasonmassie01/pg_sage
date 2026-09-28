package sre

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore implements Store on the sage.sre_* tables. All times
// come from the database clock (clock_timestamp()).
type PostgresStore struct {
	pool   *pgxpool.Pool
	limits Limits
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore binds a store to the coordination database.
func NewPostgresStore(pool *pgxpool.Pool, limits Limits) (*PostgresStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidRequest)
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &PostgresStore{pool: pool, limits: limits}, nil
}

// storeErr classifies a database error. Errors without a server
// response (connection refused, reset, pool closed) are metadata
// outages unless the caller's context ended; constraint violations are
// invalid requests.
func storeErr(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{ErrInvalidRequest, ErrNotFound, ErrLeaseUnavailable,
		ErrLeaseLost, ErrTerminal, ErrInvalidTransition, ErrVersionConflict,
		ErrBudgetExhausted, ErrUsageExceeded, ErrMetadataUnavailable} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503", "23514", "22P02", "23502":
			return fmt.Errorf("%w: %s: %s", ErrInvalidRequest, op, pgErr.Message)
		}
		return fmt.Errorf("sre store %s: %w", op, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("sre store %s: %w", op, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrMetadataUnavailable, op, err)
}

// inTx runs fn in a transaction and classifies its error. A
// commitThen error commits the transaction before it is returned.
func (s *PostgresStore) inTx(ctx context.Context, op string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storeErr(ctx, op, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	err = fn(tx)
	var ct commitThen
	if err != nil && !errors.As(err, &ct) {
		return storeErr(ctx, op, err)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return storeErr(ctx, op, cerr)
	}
	if ct.err != nil {
		return storeErr(ctx, op, ct.err)
	}
	return nil
}

// commitThen wraps an error whose transaction must still commit (an
// orphaned lease is charged before the claim is refused).
type commitThen struct{ err error }

func (c commitThen) Error() string { return c.err.Error() }

// Ping verifies the coordination tables are reachable.
func (s *PostgresStore) Ping(ctx context.Context) error {
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.sre_investigations WHERE false").Scan(&n)
	return storeErr(ctx, "ping", err)
}

// EnsureDeployment returns this deployment's UUID, creating it once.
func (s *PostgresStore) EnsureDeployment(ctx context.Context) (UUID, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO sage.sre_deployments (deployment_id)
		VALUES ($1) ON CONFLICT (singleton) DO NOTHING`, string(NewUUID())); err != nil {
		return "", storeErr(ctx, "ensure deployment", err)
	}
	var id string
	err := s.pool.QueryRow(ctx,
		"SELECT deployment_id::text FROM sage.sre_deployments").Scan(&id)
	return UUID(id), storeErr(ctx, "read deployment", err)
}

// BindDatabase returns the stable database UUID of a runtime key,
// creating the binding once.
func (s *PostgresStore) BindDatabase(ctx context.Context, b Binding) (Scope, error) {
	if err := b.validate(); err != nil {
		return Scope{}, err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.sre_database_bindings
		(deployment_id, database_id, runtime_key, legacy_database_id,
		 identity_strength, cluster_epoch)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (deployment_id, runtime_key) DO NOTHING`,
		string(b.DeploymentID), string(NewUUID()), b.RuntimeKey, b.LegacyDatabaseID,
		string(b.Strength), b.ClusterEpoch)
	if err != nil {
		return Scope{}, storeErr(ctx, "bind database", err)
	}
	var db string
	err = s.pool.QueryRow(ctx, `SELECT database_id::text FROM sage.sre_database_bindings
		WHERE deployment_id = $1 AND runtime_key = $2`,
		string(b.DeploymentID), b.RuntimeKey).Scan(&db)
	return Scope{DeploymentID: b.DeploymentID, DatabaseID: UUID(db)},
		storeErr(ctx, "read binding", err)
}

const invColumns = `deployment_id::text, database_id::text, id::text, source_case_id,
	trigger_kind, state, version, fence_token, COALESCE(lease_owner::text, ''),
	lease_until, segment_deadline, active_ms, probe_count, model_turns,
	created_at, updated_at, expires_at, COALESCE(failure_code, ''),
	COALESCE(source_incident_id, ''), subject, pinned, summary::text, concluded_at,
	evidence_purged_at`

func scanInvestigation(row pgx.Row) (Investigation, error) {
	var inv Investigation
	var dep, db, id, owner, kind, state, summary string
	var until, deadline, concluded, purged *time.Time
	err := row.Scan(&dep, &db, &id, &inv.CaseID, &kind, &state, &inv.Version,
		&inv.Fence, &owner, &until, &deadline, &inv.ActiveMS, &inv.ProbeCount,
		&inv.ModelTurns, &inv.CreatedAt, &inv.UpdatedAt, &inv.ExpiresAt,
		&inv.FailureCode, &inv.IncidentID, &inv.Subject, &inv.Pinned, &summary,
		&concluded, &purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return inv, ErrNotFound
	}
	if err != nil {
		return inv, err
	}
	if err := json.Unmarshal([]byte(summary), &inv.Summary); err != nil {
		return inv, fmt.Errorf("investigation %s summary: %w", id, err)
	}
	inv.Scope = Scope{DeploymentID: UUID(dep), DatabaseID: UUID(db)}
	inv.ID, inv.LeaseOwner = UUID(id), UUID(owner)
	inv.TriggerKind, inv.State = TriggerKind(kind), State(state)
	inv.LeaseUntil, inv.SegmentDeadline = timeOrZero(until), timeOrZero(deadline)
	inv.ConcludedAt, inv.EvidencePurgedAt = timeOrZero(concluded), timeOrZero(purged)
	return inv, nil
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func validateIDs(scope Scope, ids ...UUID) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := ParseUUID(string(id)); err != nil {
			return err
		}
	}
	return nil
}

// Get returns one investigation within its scope.
func (s *PostgresStore) Get(ctx context.Context, scope Scope, id UUID) (Investigation, error) {
	if err := validateIDs(scope, id); err != nil {
		return Investigation{}, err
	}
	inv, err := scanInvestigation(s.pool.QueryRow(ctx, `SELECT `+invColumns+`
		FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)))
	return inv, storeErr(ctx, "get", err)
}
