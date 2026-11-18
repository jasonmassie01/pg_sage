package indexes

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// DropSet is a consolidation set to measure: the named indexes of
// Schema.Table hidden together, planned over the table's workload. A
// guarded statement rising more than MaxRegressionPct is a regression.
type DropSet struct {
	Schema, Table    string
	Indexes          []string
	Queries          []optimizer.QueryInfo
	MaxRegressionPct float64
}

// Measurer validates a drop set with hypothetical indexes.
type Measurer interface {
	MeasureDropSet(ctx context.Context, s DropSet) (optimizer.WhatIfResult, error)
}

// hypoMeasurer measures on the monitored database with HypoPG: read-only
// planning in one owning session (optimizer.HypoPG.MeasureDropSet).
type hypoMeasurer struct {
	pool *pgxpool.Pool
	h    *optimizer.HypoPG
}

// NewHypoPGMeasurer measures on the monitored database's pool. logFn gets
// HypoPG's availability-probe warnings.
func NewHypoPGMeasurer(pool *pgxpool.Pool, logFn func(string, string, ...any)) Measurer {
	return &hypoMeasurer{pool: pool, h: optimizer.NewHypoPG(pool, logFn)}
}

// MeasureDropSet resolves the set's index OIDs and measures it. Every
// error names hypopg, so the set's advice says what could not be checked.
func (m *hypoMeasurer) MeasureDropSet(ctx context.Context, s DropSet,
) (optimizer.WhatIfResult, error) {
	if !m.h.IsAvailable(ctx) {
		return optimizer.WhatIfResult{}, errors.New("hypopg: the extension is not " +
			"installed on this database")
	}
	refs, err := m.resolve(ctx, s)
	if err != nil {
		return optimizer.WhatIfResult{}, err
	}
	res, err := m.h.MeasureDropSet(ctx, refs, s.Queries, s.MaxRegressionPct)
	if err != nil {
		return optimizer.WhatIfResult{}, fmt.Errorf("hypopg: measure %s.%s without %v: %w",
			s.Schema, s.Table, s.Indexes, err)
	}
	return res, nil
}

// resolve reads each index's OID in a bounded read-only transaction.
func (m *hypoMeasurer) resolve(ctx context.Context, s DropSet) ([]optimizer.IndexRef,
	error) {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("hypopg: begin the index lookup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '5s'"); err != nil {
		return nil, fmt.Errorf("hypopg: bound the index lookup: %w", err)
	}
	refs := make([]optimizer.IndexRef, 0, len(s.Indexes))
	for _, name := range s.Indexes {
		var oid uint32
		err := tx.QueryRow(ctx, `/* pg_sage */ SELECT c.oid FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'i'`,
			s.Schema, name).Scan(&oid)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("hypopg: index %s.%s no longer exists", s.Schema, name)
		}
		if err != nil {
			return nil, fmt.Errorf("hypopg: look up index %s.%s: %w", s.Schema, name, err)
		}
		refs = append(refs, optimizer.IndexRef{Name: name, OID: oid})
	}
	return refs, nil
}
