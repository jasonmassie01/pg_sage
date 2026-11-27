package drill

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cloneFacts is what the clone says, read in one READ ONLY transaction;
// each probe runs in its own savepoint so one failure does not hide the
// others.
type cloneFacts struct {
	tables       map[string]tableInfo
	readable     map[string]error
	counts       map[string]int64
	sums         map[string]sample
	marker       time.Time
	markerStatus string
	markerReason string
}

// readClone reads the clone's catalog for the source's tables, probes
// each, counts the smallest and samples the ones with a primary key and a
// created_at-like column, then reads the latest committed marker.
func readClone(ctx context.Context, pool *pgxpool.Pool, src sourceFacts, inv Invariants,
	recoveryPoint time.Time, timeout time.Duration) (cloneFacts, error) {
	facts := cloneFacts{tables: map[string]tableInfo{}, readable: map[string]error{},
		counts: map[string]int64{}, sums: map[string]sample{}}
	err := readOnly(ctx, pool, timeout, func(tx pgx.Tx) error {
		if err := readCloneCatalog(ctx, tx, src, &facts); err != nil {
			return err
		}
		probeTables(ctx, tx, src, &facts)
		sampleTables(ctx, tx, src, recoveryPoint, &facts)
		readMarker(ctx, tx, src, inv, &facts)
		return nil
	})
	if err != nil {
		return cloneFacts{}, fmt.Errorf("read drill clone: %w", err)
	}
	return facts, nil
}

func readCloneCatalog(ctx context.Context, tx pgx.Tx, src sourceFacts,
	facts *cloneFacts) error {
	if len(src.tables) == 0 {
		return nil
	}
	schemas := make([]string, len(src.tables))
	names := make([]string, len(src.tables))
	for i, t := range src.tables {
		schemas[i], names[i] = t.Schema, t.Name
	}
	rows, err := tx.Query(ctx, cloneTablesSQL, schemas, names)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	tables, err := scanTables(rows)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for _, t := range tables {
		facts.tables[t.key()] = t
	}
	return nil
}

// sameShape reports whether the clone has t with the same columns.
func (f cloneFacts) sameShape(t tableInfo) bool {
	got, ok := f.tables[t.key()]
	return ok && slices.Equal(got.Columns, t.Columns)
}

// inSavepoint runs fn in a savepoint, rolling it back on error.
func inSavepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(sp); err != nil {
		return errors.Join(err, sp.Rollback(ctx))
	}
	return sp.Commit(ctx)
}

// probeTables reads one row of every table the clone has, and counts the
// smallest of those with the source's columns.
func probeTables(ctx context.Context, tx pgx.Tx, src sourceFacts, facts *cloneFacts) {
	for _, t := range src.tables {
		if _, ok := facts.tables[t.key()]; !ok {
			continue
		}
		facts.readable[t.key()] = inSavepoint(ctx, tx, func(sp pgx.Tx) error {
			rows, err := sp.Query(ctx, `/* pg_sage restore_drill v1 */ SELECT 1 FROM `+
				t.ident()+` LIMIT 1`)
			if err != nil {
				return err
			}
			rows.Close()
			return rows.Err()
		})
	}
	var shaped []tableInfo
	for _, t := range src.tables {
		if facts.sameShape(t) && facts.readable[t.key()] == nil {
			shaped = append(shaped, t)
		}
	}
	for _, t := range smallest(shaped, exactCountTables) {
		var n int64
		err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
			return sp.QueryRow(ctx, `/* pg_sage restore_drill v1 */ SELECT count(*) FROM `+
				t.ident()).Scan(&n)
		})
		if err != nil {
			facts.readable[t.key()] = fmt.Errorf("count: %w", err)
			continue
		}
		facts.counts[t.key()] = n
	}
}

// sampleTables checksums the sample of every table that has one.
func sampleTables(ctx context.Context, tx pgx.Tx, src sourceFacts, recoveryPoint time.Time,
	facts *cloneFacts) {
	for _, t := range src.tables {
		if !sampled(t) || !facts.sameShape(t) || facts.readable[t.key()] != nil {
			continue
		}
		var s sample
		err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
			return sp.QueryRow(ctx, sampleSQL(t), recoveryPoint).Scan(&s.Rows, &s.MD5)
		})
		if err != nil {
			facts.readable[t.key()] = fmt.Errorf("sample: %w", err)
			continue
		}
		facts.sums[t.key()] = s
	}
}

// readMarker finds the latest committed marker on the clone: the newest
// sage.snapshots row when the sage schema lives in the source, else the
// operator's marker query, else none (RPO unknown).
func readMarker(ctx context.Context, tx pgx.Tx, src sourceFacts, inv Invariants,
	facts *cloneFacts) {
	query, what := "", ""
	switch {
	case src.sageMarker:
		query = `/* pg_sage restore_drill v1 */ SELECT max(collected_at) FROM sage.snapshots`
		what = "sage.snapshots.collected_at"
	case inv.MarkerQuery != "":
		query, what = inv.MarkerQuery, "drill.marker_query"
	default:
		facts.markerStatus = StatusUnknown
		facts.markerReason = "no marker: the sage schema is not in this database and " +
			"the policy document names no drill.marker_query"
		return
	}
	var at *time.Time
	err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
		return sp.QueryRow(ctx, query).Scan(&at)
	})
	switch {
	case err != nil:
		facts.markerStatus = StatusFail
		facts.markerReason = fmt.Sprintf("%s could not be read on the clone: %v", what, err)
	case at == nil:
		facts.markerStatus = StatusUnknown
		facts.markerReason = what + " has no rows on the clone"
	default:
		facts.marker, facts.markerStatus = at.UTC(), StatusPass
		facts.markerReason = what + " " + at.UTC().Format(time.RFC3339)
	}
}
