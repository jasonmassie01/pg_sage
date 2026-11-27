package schema

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Deferred indexes (config-04c). A boot runs under the startup deadline
// (10 s), so on a table of millions of rows a synchronous index build is
// cancelled at every start and leaves an INVALID index. The indexes listed
// here are left out of the boot when their table is large and built by
// BuildDeferredIndexes, CONCURRENTLY, in the background.
//
// The step that builds them must not change (its checksum is pinned), so
// deferral is decided in Go: before running a step of deferredSteps,
// Bootstrap reads each listed index's state and its table's row estimate.
// When an index is absent or INVALID on a table of deferredIndexRows rows
// or more, a variant of the step, built by the same generator without
// those indexes, runs instead and the step is not recorded; it is recorded
// on the first boot where nothing is deferred. A fresh install (reltuples
// -1 or 0) always builds inline.

// deferredIndexRows is the table size (pg_class.reltuples) at which an
// index is left to the background builder.
const deferredIndexRows = 1_000_000

// deferredIndexes is the declared list the builder owns: the ledger
// indexes first. The migration ledger leaves them out of its catalog
// fingerprint and INVALID check.
var deferredIndexes = ledgerIndexes

// deferredStep is a step whose indexes may be deferred: its generator's
// columns block and index list.
type deferredStep struct {
	columns string
	indexes []ledgerIndex
}

var deferredSteps = map[string]deferredStep{
	"ddlDecisionLedger": {columns: ddlDecisionLedgerColumns, indexes: ledgerIndexes},
}

// deferredRowsFn returns the row estimate of each named sage table; a
// missing table is absent from the map. Tests replace it.
var deferredRowsFn = readDeferredRows

const deferredRowsSQL = `SELECT c.reltuples::float8 FROM pg_catalog.pg_class c
 WHERE c.relnamespace = 'sage'::regnamespace AND c.relname = $1`

func readDeferredRows(ctx context.Context, db bootstrapDB, tables []string) (
	map[string]float64, error) {
	rows := make(map[string]float64, len(tables))
	for _, table := range tables {
		var n float64
		err := db.QueryRow(ctx, deferredRowsSQL, table).Scan(&n)
		if err == nil {
			rows[table] = n
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("reading the row estimate of sage.%s: %w", table, err)
		}
	}
	return rows, nil
}

// atOrAboveDeferral reports whether a table of rows (reltuples) is large
// enough for its indexes to be built in the background.
func atOrAboveDeferral(rows float64) bool {
	return rows >= deferredIndexRows
}

// deferredIndexStateSQL reads whether each named sage index needs a build:
// absent, or INVALID.
const deferredIndexStateSQL = `SELECT to_regclass('sage.' || $1) IS NULL,
       COALESCE((SELECT NOT i.indisvalid FROM pg_catalog.pg_index i
                  WHERE i.indexrelid = to_regclass('sage.' || $1)), false)`

// indexesNeedingBuild returns the names of indexes that are absent or
// INVALID.
func indexesNeedingBuild(ctx context.Context, db bootstrapDB, names []string) (
	map[string]bool, error) {
	need := make(map[string]bool, len(names))
	for _, name := range names {
		var absent, invalid bool
		if err := db.QueryRow(ctx, deferredIndexStateSQL, name).
			Scan(&absent, &invalid); err != nil {
			return nil, fmt.Errorf("reading the state of sage.%s: %w", name, err)
		}
		need[name] = absent || invalid
	}
	return need, nil
}

// planDeferral returns the indexes of step to leave to the builder: each
// declared deferred index the step builds that is absent or INVALID on a
// table at or above the threshold. None for any other step.
func planDeferral(ctx context.Context, db bootstrapDB, step migrationStep) ([]string, error) {
	spec, ok := deferredSteps[step.name]
	if !ok {
		return nil, nil
	}
	var names, tables []string
	for _, idx := range spec.indexes {
		if isDeferredIndex(idx.name) {
			names = append(names, idx.name)
			tables = append(tables, idx.table)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := deferredRowsFn(ctx, db, slices.Compact(slices.Sorted(slices.Values(tables))))
	if err != nil {
		return nil, fmt.Errorf("migration step %s: %w", step.name, err)
	}
	need, err := indexesNeedingBuild(ctx, db, names)
	if err != nil {
		return nil, fmt.Errorf("migration step %s: %w", step.name, err)
	}
	var deferred []string
	for i, name := range names {
		n, exists := rows[tables[i]]
		if exists && need[name] && atOrAboveDeferral(n) {
			deferred = append(deferred, name)
		}
	}
	return deferred, nil
}

func isDeferredIndex(name string) bool {
	return slices.ContainsFunc(deferredIndexes, func(i ledgerIndex) bool { return i.name == name })
}

// deferredVariant is step without the deferred indexes, built by the
// step's own generator. ok is false when nothing of step is deferred.
func deferredVariant(step migrationStep, deferred []string) (migrationStep, bool) {
	spec, isDeferrable := deferredSteps[step.name]
	if !isDeferrable {
		return step, false
	}
	kept := withoutIndexes(spec.indexes, deferred)
	if len(kept) == len(spec.indexes) {
		return step, false
	}
	return migrationStep{name: step.name, sql: ledgerIndexBlock(spec.columns, kept)}, true
}

// withoutIndexes is indexes minus the named ones, in order.
func withoutIndexes(indexes []ledgerIndex, names []string) []ledgerIndex {
	out := make([]ledgerIndex, 0, len(indexes))
	for _, idx := range indexes {
		if !slices.Contains(names, idx.name) {
			out = append(out, idx)
		}
	}
	return out
}

// runDeferredVariant runs step without its deferred indexes when any is
// deferred. ran is false when nothing is deferred: the caller runs and
// records the step as usual. The variant is never recorded, so the step's
// recorded checksum stays the released one.
func runDeferredVariant(ctx context.Context, db bootstrapDB, step migrationStep) (
	stepOutcome, bool, error) {
	deferred, err := planDeferral(ctx, db, step)
	if err != nil || len(deferred) == 0 {
		return stepSkipped, false, err
	}
	variant, ok := deferredVariant(step, deferred)
	if !ok {
		return stepSkipped, false, nil
	}
	slog.Info("schema: large sage indexes left to the background builder",
		"step", step.name, "indexes", strings.Join(deferred, ","))
	outcome, err := runCheckedStep(ctx, db, variant)
	if err != nil {
		return outcome, true, err
	}
	ledgerFrom(ctx).noteRan()
	return outcome, true, nil
}

// deferredIndexNamesSQL is the declared deferred index names as a SQL
// list of literals, for the ledger's catalog queries. The names are code
// constants, not input.
func deferredIndexNamesSQL() string {
	lits := make([]string, 0, len(deferredIndexes))
	for _, idx := range deferredIndexes {
		lits = append(lits, sqlLiteral(idx.name))
	}
	return "(" + strings.Join(lits, ", ") + ")"
}
