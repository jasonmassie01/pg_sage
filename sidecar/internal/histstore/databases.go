package histstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// StoreDatabase is one database that keeps its history in the meta
// database, with its last known size (the snapshot cap's input).
type StoreDatabase struct {
	ID        int
	Name      string
	DBBytes   int64 // 0 when never measured
	UpdatedAt time.Time
}

var errNotScoped = errors.New("histstore: only a meta store has a registry row")

// RegisterDatabase records that the store's database keeps its history
// there, under name.
func RegisterDatabase(ctx context.Context, s Store, name string) error {
	if !s.Scoped() {
		return errNotScoped
	}
	db, ok := s.db.(execer)
	if !ok {
		return s.noDB()
	}
	_, err := db.Exec(ctx, `/* pg_sage */ INSERT INTO sage.history_store_databases
		(database_id, database_name) VALUES ($1, $2)
		ON CONFLICT (database_id) DO UPDATE SET database_name = EXCLUDED.database_name,
		updated_at = now()`, s.databaseID, name)
	if err != nil {
		return fmt.Errorf("register database %d (%q) in the history store: %w",
			s.databaseID, name, err)
	}
	return nil
}

// UpdateDatabaseSize records the database's current size.
func UpdateDatabaseSize(ctx context.Context, s Store, bytes int64) error {
	if !s.Scoped() {
		return errNotScoped
	}
	db, ok := s.db.(execer)
	if !ok {
		return s.noDB()
	}
	_, err := db.Exec(ctx, `/* pg_sage */ UPDATE sage.history_store_databases
		SET db_bytes = $2, updated_at = now() WHERE database_id = $1`, s.databaseID, bytes)
	if err != nil {
		return fmt.Errorf("record the size of database %d in the history store: %w",
			s.databaseID, err)
	}
	return nil
}

// StoreDatabases lists the databases registered in the meta database db
// and updated since since, by id.
func StoreDatabases(ctx context.Context, db DB, since time.Time) ([]StoreDatabase, error) {
	if isNil(db) {
		return nil, ErrNoDatabase
	}
	rows, err := db.Query(ctx, `/* pg_sage */ SELECT database_id, database_name,
		COALESCE(db_bytes, 0), updated_at FROM sage.history_store_databases
		WHERE updated_at >= $1 ORDER BY database_id`, since)
	if err != nil {
		return nil, fmt.Errorf("list history store databases: %w", err)
	}
	defer rows.Close()
	var out []StoreDatabase
	for rows.Next() {
		var d StoreDatabase
		if err := rows.Scan(&d.ID, &d.Name, &d.DBBytes, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan history store database: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// historyShareSQL reads one history table's size, its estimated row count
// over all databases and this database's exact row count (an index-only
// count on the database_id index).
var historyShareSQL = map[string]string{
	"snapshots": `/* pg_sage */ SELECT
		(SELECT COALESCE(sum(pg_total_relation_size(relid)), 0)::float8
		   FROM pg_partition_tree('sage.snapshots')),
		(SELECT COALESCE(sum(GREATEST(c.reltuples, 0)), 0)::float8
		   FROM pg_partition_tree('sage.snapshots') pt
		   JOIN pg_class c ON c.oid = pt.relid WHERE pt.isleaf),
		(SELECT count(*) FROM sage.snapshots s WHERE {db:s})`,
	"query_store": `/* pg_sage */ SELECT
		(SELECT COALESCE(sum(pg_total_relation_size(relid)), 0)::float8
		   FROM pg_partition_tree('sage.query_store')),
		(SELECT COALESCE(sum(GREATEST(c.reltuples, 0)), 0)::float8
		   FROM pg_partition_tree('sage.query_store') pt
		   JOIN pg_class c ON c.oid = pt.relid WHERE pt.isleaf),
		(SELECT count(*) FROM sage.query_store q WHERE {db:q})`,
}

// rollupShareSQL is historyShareSQL for an unpartitioned rollup table
// (evid-01b): its size, its estimated rows and this database's rows ($1,
// counted on the COALESCE(database_id, 0) index).
func rollupShareSQL(table string) string {
	return `/* pg_sage */ SELECT
		COALESCE(pg_total_relation_size(to_regclass('sage.` + table + `')), 0)::float8,
		(SELECT GREATEST(c.reltuples, 0)::float8 FROM pg_class c
		  WHERE c.oid = to_regclass('sage.` + table + `')),
		(SELECT count(*) FROM sage.` + table + ` r WHERE COALESCE(r.database_id, 0) = $1)`
}

func init() {
	for _, t := range RollupTables {
		historyShareSQL[t] = rollupShareSQL(t)
	}
}

// historyBytesTables are the tables HistoryBytes measures, in order.
var historyBytesTables = append([]string{"snapshots", "query_store"}, RollupTables...)

// HistoryBytes is this database's share of the store's history: each
// history and rollup table's size times the database's share of its rows.
// A monitored store returns 0: its history is part of the sage schema the
// self-cost reading already measures.
func (s Store) HistoryBytes(ctx context.Context) (int64, error) {
	if !s.Scoped() {
		return 0, nil
	}
	return s.ShareBytes(ctx, historyBytesTables)
}

// ShareBytes is this database's share of the named sage tables' size (a
// meta store; 0 otherwise).
func (s Store) ShareBytes(ctx context.Context, tables []string) (int64, error) {
	if !s.Scoped() {
		return 0, nil
	}
	var total float64
	for _, table := range tables {
		var size float64
		var all *float64
		var mine int64
		var args []any
		if slices.Contains(RollupTables, table) {
			args = []any{s.databaseID}
		}
		if err := s.QueryRow(ctx, historyShareSQL[table], args...).Scan(&size, &all,
			&mine); err != nil {
			return 0, fmt.Errorf("measure database %d's share of sage.%s: %w",
				s.databaseID, table, err)
		}
		// Statistics lag the rows: a share is never above the whole table.
		whole := float64(mine)
		if all != nil {
			whole = max(*all, whole)
		}
		if whole > 0 {
			total += size * float64(mine) / whole
		}
	}
	return int64(total), nil
}
