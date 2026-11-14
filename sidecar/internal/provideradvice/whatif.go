package provideradvice

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// maxWhatIfStatements bounds the workload a provider index is measured on.
const maxWhatIfStatements = 20

const tableIndexesSQL = `SELECT c.relname, pg_catalog.pg_get_indexdef(i.indexrelid)
FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = $1::regclass AND i.indisvalid ORDER BY c.relname`

// rel is the qualified pg_stat_statements view.
func tableWorkloadSQL(rel string) string {
	return `SELECT queryid, query, calls, mean_exec_time, total_exec_time
FROM ` + rel + `
WHERE dbid = (SELECT oid FROM pg_catalog.pg_database WHERE datname = current_database())
  AND position(lower($1) IN lower(query)) > 0
ORDER BY total_exec_time DESC LIMIT $2`
}

// WhatIf says whether pg_sage would act on a provider-only index and
// why, failing closed: an index an existing one serves, an unmodelable
// method, no HypoPG, no workload or a failed measurement are each "would
// not act". Acting stays pg_sage's own optimizer's and policy gate's call.
func (e *DBEvaluator) WhatIf(ctx context.Context, ddl string) (bool, string) {
	spec, err := optimizer.ParseIndexDDL(ddl)
	if err != nil {
		return false, "would not act: the statement does not parse: " + err.Error()
	}
	schema := spec.TableSchema
	if schema == "" {
		schema = "public"
	}
	table := pgx.Identifier{schema, spec.TableName}.Sanitize()
	queries, served, err := e.whatIfInputs(ctx, ddl, table, spec.TableName)
	switch {
	case errors.Is(err, errNoTable):
		return false, "would not act: table " + table + " does not exist in this database"
	case err != nil:
		return false, "would not act: pg_sage could not read the table's indexes and " +
			"workload: " + err.Error()
	case served != "":
		return false, "would not act: existing index " + served + " already serves it"
	}
	if reason := optimizer.HypoPGCannotModel(ddl); reason != "" {
		return false, "would not act: " + reason
	}
	if !e.hypo.IsAvailable(ctx) {
		return false, "would not act: HypoPG is not installed, so pg_sage cannot measure " +
			"the gain"
	}
	if len(queries) == 0 {
		return false, "would not act: no pg_stat_statements statement reads " + table
	}
	return e.measure(ctx, ddl, table, queries)
}

// whatIfInputs reads, in one bounded read-only transaction, the existing
// index that already serves ddl (if any) and the table's hottest
// statements. pg_stat_statements is optional: without it the workload is
// empty.
func (e *DBEvaluator) whatIfInputs(ctx context.Context, ddl, table, name string,
) ([]optimizer.QueryInfo, string, error) {
	var queries []optimizer.QueryInfo
	var served string
	err := e.readOnly(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).
			Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errNoTable
		}
		var err error
		if served, err = servingIndex(ctx, tx, ddl, table); err != nil || served != "" {
			return err
		}
		queries, err = e.tableWorkload(ctx, tx, name)
		return err
	})
	return queries, served, err
}

func servingIndex(ctx context.Context, tx pgx.Tx, ddl, table string) (string, error) {
	rows, err := tx.Query(ctx, tableIndexesSQL, table)
	if err != nil {
		return "", fmt.Errorf("list indexes of %s: %w", table, err)
	}
	defs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Name, Def string }])
	if err != nil {
		return "", fmt.Errorf("list indexes of %s: %w", table, err)
	}
	for _, d := range defs {
		if optimizer.CoveredBy(ddl, d.Def) {
			return d.Name, nil
		}
	}
	return "", nil
}

func (e *DBEvaluator) tableWorkload(ctx context.Context, tx pgx.Tx, name string,
) ([]optimizer.QueryInfo, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT workload"); err != nil {
		return nil, err
	}
	rel, err := extschema.StatementsRel(ctx, tx, "pg_stat_statements")
	var rows pgx.Rows
	if err == nil {
		rows, err = tx.Query(ctx, tableWorkloadSQL(rel), name, maxWhatIfStatements)
	}
	var out []optimizer.QueryInfo
	if err == nil {
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (optimizer.QueryInfo,
			error) {
			var q optimizer.QueryInfo
			err := row.Scan(&q.QueryID, &q.Text, &q.Calls, &q.MeanTimeMs, &q.TotalTimeMs)
			return q, err
		})
	}
	if err != nil { // pg_stat_statements absent or unreadable: no workload
		e.logFn("INFO", "provider advice: no workload for the what-if of %s: %v", name, err)
		_, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT workload")
		return nil, rbErr
	}
	return out, nil
}

func (e *DBEvaluator) measure(ctx context.Context, ddl, table string,
	queries []optimizer.QueryInfo) (bool, string) {
	res, err := e.hypo.Validate(ctx, optimizer.Recommendation{Table: table, DDL: ddl},
		queries)
	verdict, reason := optimizer.WhatIfVerdict(res, err, config.DefaultOptHypoPGMinImprovePct)
	if verdict == optimizer.WhatIfVerified {
		return true, fmt.Sprintf("would act: HypoPG estimates %.0f%% lower planner cost "+
			"on %d statements; it still goes through pg_sage's optimizer and the policy gate",
			res.Improvement, res.Measured)
	}
	return false, "would not act: " + reason
}
