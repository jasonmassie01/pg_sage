package api

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/workload"
)

// The query-hint cases of one database: the bounded read, the
// pg_stat_statements text annotation and the self-monitoring filter.

func queryHintCases(
	ctx context.Context,
	selected namedPool,
) ([]cases.Case, bool, error) {
	rows, err := queryQueryHintsLimit(ctx, selected.pool, caseQueryHintsCap+1, "")
	if err != nil {
		return nil, false, err
	}
	cut := len(rows) > caseQueryHintsCap
	if cut {
		rows = rows[:caseQueryHintsCap]
	}
	return projectHintRows(ctx, selected, rows), cut, nil
}

func projectHintRows(
	ctx context.Context, selected namedPool, rows []map[string]any,
) []cases.Case {
	annotateQueryHintTexts(ctx, selected.pool, rows)
	rows = filterSelfMonitoringHintRows(rows)
	out := make([]cases.Case, 0, len(rows))
	for _, row := range rows {
		if stringValue(row["status"]) == "retired" {
			continue
		}
		row["database_name"] = selected.name
		out = append(out, cases.ProjectQueryHint(sourceQueryHintFromMap(row)))
	}
	return out
}

func annotateQueryHintTexts(
	ctx context.Context,
	pool *pgxpool.Pool,
	rows []map[string]any,
) {
	queryIDs := uniqueQueryHintIDs(rows)
	if len(queryIDs) == 0 {
		return
	}
	rel, err := extschema.StatementsRel(ctx, pool, "pg_stat_statements")
	var psRows pgx.Rows
	if err == nil {
		psRows, err = pool.Query(ctx,
			`/* pg_sage */ SELECT queryid, min(query)
		 FROM `+rel+`
		 WHERE dbid = (
		   SELECT oid FROM pg_database WHERE datname = current_database()
		 )
		   AND queryid = ANY($1::bigint[])
		 GROUP BY queryid`,
			queryIDs,
		)
	}
	if err != nil {
		// Don't fail silently: without query_text the self-monitoring
		// filter downstream can't recognize pg_sage's own hints, so they
		// would leak into the UI. Surface the cause (D2).
		slog.Warn("query-hint text annotation failed",
			"error", err)
		return
	}
	defer psRows.Close()

	textByID := make(map[int64]string, len(queryIDs))
	for psRows.Next() {
		var queryID int64
		var queryText string
		if err := psRows.Scan(&queryID, &queryText); err == nil {
			textByID[queryID] = queryText
		}
	}
	for _, row := range rows {
		if queryText := textByID[queryHintID(row)]; queryText != "" {
			row["query_text"] = queryText
		}
	}
}

func uniqueQueryHintIDs(rows []map[string]any) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, row := range rows {
		queryID := queryHintID(row)
		if queryID == 0 || seen[queryID] {
			continue
		}
		seen[queryID] = true
		out = append(out, queryID)
	}
	return out
}

func queryHintID(row map[string]any) int64 {
	// queryid is a 64-bit pg_stat_statements hash and is stored as an
	// int64 (see scanQueryHintRows). It must NOT go through float64:
	// values beyond 2^53 lose precision, so the ANY($1::bigint[]) lookup
	// in annotateQueryHintTexts would miss and the self-monitoring
	// filter would silently fail (D1).
	return int64Value(row["queryid"])
}

func filterSelfMonitoringHintRows(
	rows []map[string]any,
) []map[string]any {
	out := rows[:0]
	for _, row := range rows {
		if workload.Excluded(stringValue(row["query_text"])) {
			continue
		}
		out = append(out, row)
	}
	return out
}
