package analyzer

import (
	"context"
	"errors"
)

// indexBuildsSQL lists tables of this database with an index build in
// progress (CREATE INDEX [CONCURRENTLY], REINDEX [CONCURRENTLY]). A NULL
// relname means the row is not visible to this role.
const indexBuildsSQL = `/* pg_sage */ SELECT n.nspname, c.relname
  FROM pg_stat_progress_create_index p
  LEFT JOIN pg_class c ON c.oid = p.relid
  LEFT JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE p.datid = (SELECT oid FROM pg_database
                   WHERE datname = current_database())`

// loadIndexBuilds refreshes extras.IndexBuildTables (G2-B09). It fails
// closed: on a query error, or when a build row is not attributable to a
// table, IndexBuildProbeFailed suppresses invalid-index findings and
// invalid_index is not resolved this cycle.
func (a *Analyzer) loadIndexBuilds(ctx context.Context) {
	tables, err := a.queryIndexBuilds(ctx)
	a.extras.IndexBuildTables = tables
	a.extras.IndexBuildProbeFailed = err != nil
	if err != nil {
		a.logFn("WARN", "analyzer: index build probe: %v", err)
		a.evalFail("invalid_index")
	}
}

func (a *Analyzer) queryIndexBuilds(ctx context.Context) (map[string]bool, error) {
	rows, err := a.pool.Query(ctx, indexBuildsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make(map[string]bool)
	for rows.Next() {
		var schema, table *string
		if err := rows.Scan(&schema, &table); err != nil {
			return nil, err
		}
		if schema == nil || table == nil {
			return nil, errUnattributedIndexBuild
		}
		tables[*schema+"."+*table] = true
	}
	return tables, rows.Err()
}

var errUnattributedIndexBuild = errors.New(
	"index build in progress on a relation not visible to this role")
