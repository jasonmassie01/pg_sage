package upgradecheck

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/workload"
)

// The query-time share (drill-02c): pg_upgrade leaves pg_stat_statements
// empty, so the post-upgrade ANALYZE is ordered by what the brief recorded
// before the upgrade: each table's share of the statements' total time.

// maxShareQueryIDs bounds the statements recorded per table.
const maxShareQueryIDs = 5

// TableShare is one table's query-time share: the total time of the
// statements that name it over the total time of every statement read (a
// join counts toward each table it names), with its busiest statements.
type TableShare struct {
	Table    string  `json:"table"`
	Share    float64 `json:"share"`
	QueryIDs []int64 `json:"queryids,omitempty"`
}

// QueryTimeSection lists the busiest tables, at most MaxRows; Total
// counts every table with a share.
type QueryTimeSection struct {
	Section
	Items []TableShare `json:"items"`
	Total int          `json:"total"`
}

// statementTime is one pg_stat_statements row.
type statementTime struct {
	QueryID int64
	Text    string
	TotalMs float64
}

// knownTable is a user table by its catalog names and as format('%I.%I').
type knownTable struct {
	Schema, Name, Qualified string
}

// tableShares attributes each statement's time to the known tables it
// names; the busiest MaxRows tables are kept, and the total counts all.
func tableShares(stmts []statementTime, tables []knownTable) ([]TableShare, int) {
	known := map[string][]string{}
	qualified := map[workload.Relation]string{}
	for _, t := range tables {
		known[t.Name] = append(known[t.Name], t.Schema)
		qualified[workload.Relation{Schema: t.Schema, Name: t.Name}] = t.Qualified
	}
	var total float64
	byTable := map[string]*TableShare{}
	for _, s := range stmts {
		total += s.TotalMs
		for _, rel := range workload.ReferencedTables(s.Text, known) {
			name := qualified[rel]
			ts := byTable[name]
			if ts == nil {
				ts = &TableShare{Table: name}
				byTable[name] = ts
			}
			ts.Share += s.TotalMs
			if len(ts.QueryIDs) < maxShareQueryIDs && !slices.Contains(ts.QueryIDs, s.QueryID) {
				ts.QueryIDs = append(ts.QueryIDs, s.QueryID)
			}
		}
	}
	if total <= 0 {
		return []TableShare{}, 0
	}
	out := make([]TableShare, 0, len(byTable))
	for _, ts := range byTable {
		ts.Share /= total
		out = append(out, *ts)
	}
	slices.SortFunc(out, func(a, b TableShare) int {
		return cmp.Or(cmp.Compare(b.Share, a.Share), cmp.Compare(a.Table, b.Table))
	})
	n := len(out)
	if n > MaxRows {
		out = out[:MaxRows]
	}
	return out, n
}

// queryTimeSection is the brief's section from the facts.
func queryTimeSection(f briefFacts) QueryTimeSection {
	switch {
	case errors.Is(f.sharesErr, extschema.ErrExtensionAbsent):
		return QueryTimeSection{Items: []TableShare{}, Section: Section{State: SectionUnknown,
			Reason: "pg_stat_statements is not installed in this database: the ANALYZE " +
				"after the upgrade is ordered by table size"}}
	case f.sharesErr != nil:
		return QueryTimeSection{Items: []TableShare{}, Section: unknownSection(
			"pg_stat_statements could not be read; the ANALYZE after the upgrade is "+
				"ordered by table size", f.sharesErr)}
	}
	items := f.shares
	if items == nil {
		items = []TableShare{}
	}
	return QueryTimeSection{Items: items, Total: f.sharesTotal, Section: Section{
		State:  SectionKnown,
		Reason: "share of this database's pg_stat_statements total time, per table named"}}
}

// shareScanLimit bounds the statements read, by total time.
const shareScanLimit = 1000

const shareTablesSQL = Tag + `SELECT n.nspname, c.relname,
  pg_catalog.format('%I.%I', n.nspname, c.relname)
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'm') AND c.relpersistence <> 't'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast'`

// readTableShares reads the share in the brief's transaction; a database
// without pg_stat_statements is extschema.ErrExtensionAbsent.
func readTableShares(ctx context.Context, tx pgx.Tx) ([]TableShare, int, error) {
	var out []TableShare
	var total int
	err := catalogread.InSavepoint(ctx, tx, func(sp pgx.Tx) error {
		rel, err := extschema.StatementsRel(ctx, sp, "pg_stat_statements")
		if err != nil {
			return err
		}
		stmts, err := readStatementTimes(ctx, sp, rel)
		if err != nil {
			return err
		}
		tables, err := readKnownTables(ctx, sp)
		if err != nil {
			return err
		}
		out, total = tableShares(stmts, tables)
		return nil
	})
	return out, total, err
}

func readStatementTimes(ctx context.Context, tx pgx.Tx, rel string) ([]statementTime,
	error) {
	rows, err := tx.Query(ctx, Tag+`SELECT queryid, query, total_exec_time FROM `+rel+`
		WHERE dbid = (SELECT oid FROM pg_catalog.pg_database
		              WHERE datname = current_database())
		  AND queryid IS NOT NULL AND query IS NOT NULL
		ORDER BY total_exec_time DESC, queryid LIMIT $1`, shareScanLimit)
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (statementTime, error) {
		var s statementTime
		err := r.Scan(&s.QueryID, &s.Text, &s.TotalMs)
		return s, err
	})
}

func readKnownTables(ctx context.Context, tx pgx.Tx) ([]knownTable, error) {
	rows, err := tx.Query(ctx, shareTablesSQL)
	if err != nil {
		return nil, fmt.Errorf("read the tables: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (knownTable, error) {
		var k knownTable
		err := r.Scan(&k.Schema, &k.Name, &k.Qualified)
		return k, err
	})
}
