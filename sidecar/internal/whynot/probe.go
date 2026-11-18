package whynot

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// Querier is what the probe needs from a pool: a read-only transaction.
type Querier interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// probeSQL reads the role, pg_stat_statements' presence, whether the role
// sees every role's query text (rolegrants.QueryTextVisibleSQL, the test
// the first look and the startup checks make) and recovery. Catalog reads
// only.
const probeSQL = `/* pg_sage */ SELECT current_user::text,
	EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = 'pg_stat_statements'),
	` + rolegrants.QueryTextVisibleSQL + `,
	pg_catalog.pg_is_in_recovery()`

// ProbePrivileges reads the privilege link's facts from the monitored
// database in a read-only transaction bounded by a statement and a lock
// timeout. It reads afresh every call, so a grant shows on the next read.
func ProbePrivileges(ctx context.Context, q Querier) (*Privileges, error) {
	if q == nil {
		return nil, errors.New("probe privileges: no database pool")
	}
	tx, err := q.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("probe privileges: begin read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '2s'"); err != nil {
		return nil, fmt.Errorf("probe privileges: set statement_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '500ms'"); err != nil {
		return nil, fmt.Errorf("probe privileges: set lock_timeout: %w", err)
	}
	var p Privileges
	if err := tx.QueryRow(ctx, probeSQL).Scan(&p.Role, &p.StatStatements,
		&p.QueryTextVisible, &p.Replica); err != nil {
		return nil, fmt.Errorf("probe privileges: read catalog: %w", err)
	}
	return &p, nil
}
