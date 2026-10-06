package histstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
)

// The migration copies one database's history from one placement to the
// other: monitored -> meta (history.store: meta) or back. The source is
// only read; each destination batch commits with its progress row in
// sage.history_migration, so a run resumes exactly where the last one
// stopped and a re-run copies only rows written since. Snapshots are
// copied in id order (a delta's base always has a lower id) with new ids
// assigned up front, so each delta's base is remapped through
// sage.history_migration_ids. Cleanup removes the source rows, and only
// once every row is copied.

// historyTables are the tables that move, in copy order.
var historyTables = []string{"snapshots", "query_store"}

// Errors a caller can tell apart.
var (
	ErrSamePlacement = errors.New("histstore: source and destination are the same " +
		"placement: migrate from monitored to meta or back")
	ErrMigrationBusy = errors.New("histstore: another migration of this database is " +
		"running")
	ErrIncomplete = errors.New("histstore: the copy is not complete; run the migration " +
		"to the end before cleaning up")
	ErrUnknownColumns = errors.New("histstore: the history table has columns this " +
		"version does not copy; upgrade before migrating")
)

// DefaultBatchRows is the rows one destination transaction copies.
const DefaultBatchRows = 500

// MigrateOptions tune a run. MaxBatches stops after that many batches (0:
// run to the end); a stopped run is resumed by the next one.
type MigrateOptions struct {
	BatchRows  int
	MaxBatches int
	Log        func(format string, args ...any)
}

// TableReport is what a run did for one table. Complete: every source row
// is copied (or skipped as unreadable).
type TableReport struct {
	Table    string
	Copied   int64
	Skipped  int64
	Complete bool
}

// MigrateReport is a run's result per table.
type MigrateReport struct {
	Direction  Mode
	DatabaseID int
	Tables     []TableReport
}

// mark is a table's progress in the destination.
type mark struct {
	found     bool
	lastID    int64
	lastAt    *time.Time
	completed bool
}

// migration is one run's state.
type migration struct {
	src, dst Store
	dir      Mode
	id       int
	conn     *pgxpool.Conn // destination, holding the run's advisory lock
	opt      MigrateOptions
	batches  int // committed this run
}

// Migrate copies src's history to dst. One of them must be the meta store
// and the other the monitored database.
func Migrate(ctx context.Context, src, dst Store, opt MigrateOptions) (MigrateReport,
	error) {
	m, err := startMigration(ctx, src, dst, opt)
	if err != nil {
		return MigrateReport{}, err
	}
	defer m.release(ctx)
	rep := MigrateReport{Direction: m.dir, DatabaseID: m.id}
	for _, table := range historyTables {
		tr, err := m.copyTable(ctx, table)
		if err != nil {
			return rep, err
		}
		rep.Tables = append(rep.Tables, tr)
	}
	return rep, nil
}

// placement checks the pair and returns the direction and database id.
func placement(src, dst Store) (Mode, int, error) {
	if isNil(src.db) || isNil(dst.db) {
		return "", 0, ErrNoDatabase
	}
	if src.Scoped() == dst.Scoped() {
		return "", 0, ErrSamePlacement
	}
	if dst.Scoped() {
		return ModeMeta, dst.databaseID, nil
	}
	return ModeMonitored, src.databaseID, nil
}

func startMigration(ctx context.Context, src, dst Store, opt MigrateOptions) (*migration,
	error) {
	dir, id, err := placement(src, dst)
	if err != nil {
		return nil, err
	}
	if opt.BatchRows < 0 || opt.MaxBatches < 0 {
		return nil, fmt.Errorf("histstore: batch rows and max batches must be >= 0, got "+
			"%d and %d", opt.BatchRows, opt.MaxBatches)
	}
	if opt.BatchRows == 0 {
		opt.BatchRows = DefaultBatchRows
	}
	if src, err = withLegacy(ctx, src); err != nil {
		return nil, err
	}
	if dst, err = withLegacy(ctx, dst); err != nil {
		return nil, err
	}
	pool, ok := dst.db.(*pgxpool.Pool)
	if !ok {
		return nil, fmt.Errorf("histstore: the destination must be a pool, got %T", dst.db)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("histstore: connect to the destination: %w", err)
	}
	m := &migration{src: src, dst: dst.WithDB(conn), dir: dir, id: id, conn: conn, opt: opt}
	if err := m.prepare(ctx); err != nil {
		m.release(ctx)
		return nil, err
	}
	return m, nil
}

// withLegacy scopes a monitored side to rows without a database id when
// its tables have the column (the meta database monitored as itself).
func withLegacy(ctx context.Context, s Store) (Store, error) {
	db, ok := s.db.(DB)
	if s.Scoped() || s.legacyNull || !ok {
		return s, nil
	}
	has, err := hasIdentity(ctx, db)
	if err != nil {
		return s, err
	}
	s.legacyNull = has
	return s, nil
}

// lockKey serializes migrations of one database in one direction.
const lockKey = "sage.history_migration"

// prepare takes the run's lock, creates the progress tables and checks
// both sides' columns.
func (m *migration) prepare(ctx context.Context) error {
	var locked bool
	if err := m.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1 || ':' || $2),
		$3)`, lockKey, string(m.dir), m.id).Scan(&locked); err != nil {
		return fmt.Errorf("histstore: lock the migration: %w", err)
	}
	if !locked {
		m.conn.Release()
		m.conn = nil
		return ErrMigrationBusy
	}
	if err := schema.EnsureHistoryMigrationTables(ctx, m.conn); err != nil {
		return err
	}
	for _, table := range historyTables {
		for _, side := range []Store{m.src, m.dst} {
			if err := checkColumns(ctx, side, table); err != nil {
				return err
			}
		}
	}
	return nil
}

// release unlocks and returns the destination connection. A lock that
// cannot be released goes with its connection.
func (m *migration) release(ctx context.Context) {
	if m.conn == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := m.conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1 || ':' || $2),
		$3)`, lockKey, string(m.dir), m.id); err != nil {
		_ = m.conn.Hijack().Close(ctx)
		m.conn = nil
		return
	}
	m.conn.Release()
	m.conn = nil
}

// knownColumns are the columns the migration copies (id is reassigned,
// database_id is the placement).
var knownColumns = map[string]map[string]bool{
	"snapshots": {"id": true, "collected_at": true, "category": true, "data": true,
		"base_id": true, "database_id": true},
	"query_store": {"id": true, "captured_at": true, "queryid": true, "calls": true,
		"total_exec_time": true, "mean_exec_time": true, "rows": true, "plan_hash": true,
		"stats_epoch": true, "database_id": true},
}

// checkColumns refuses a table with a column the copy would drop, and a
// meta store without database_id.
func checkColumns(ctx context.Context, s Store, table string) error {
	rows, err := s.Query(ctx, `SELECT attname::text FROM pg_catalog.pg_attribute
		WHERE attrelid = pg_catalog.to_regclass('sage.' || $1) AND attnum > 0
		  AND NOT attisdropped`, table)
	if err != nil {
		return fmt.Errorf("histstore: read the columns of sage.%s: %w", table, err)
	}
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("histstore: read the columns of sage.%s: %w", table, err)
	}
	if len(cols) == 0 {
		return fmt.Errorf("histstore: sage.%s does not exist: %w", table,
			schema.ErrNoSageSchema)
	}
	has := map[string]bool{}
	for _, c := range cols {
		if !knownColumns[table][c] {
			return fmt.Errorf("%w: sage.%s.%s", ErrUnknownColumns, table, c)
		}
		has[c] = true
	}
	if s.Scoped() && !has["database_id"] {
		return fmt.Errorf("histstore: the meta database is not a history store "+
			"(sage.%s has no database_id): bootstrap it with history.store: meta", table)
	}
	return nil
}

// readMark reads a table's progress from db (the destination); no progress
// table means no progress.
func readMark(ctx context.Context, db any, id int, dir Mode, table string) (mark, error) {
	q, ok := db.(rowQuerier)
	if !ok || isNil(db) {
		return mark{}, ErrNoDatabase
	}
	var m mark
	var exists bool
	err := q.QueryRow(ctx, `SELECT to_regclass('sage.history_migration') IS NOT NULL`).
		Scan(&exists)
	if err != nil || !exists {
		return mark{}, err
	}
	var completedAt *time.Time
	err = q.QueryRow(ctx, `SELECT last_id, last_at, completed_at FROM sage.history_migration
		WHERE database_id = $1 AND direction = $2 AND table_name = $3`,
		id, string(dir), table).Scan(&m.lastID, &m.lastAt, &completedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return mark{}, nil
	case err != nil:
		return mark{}, fmt.Errorf("read the migration progress of sage.%s: %w", table, err)
	}
	m.found, m.completed = true, completedAt != nil
	return m, nil
}

func (m *migration) logf(format string, args ...any) {
	if m.opt.Log != nil {
		m.opt.Log(format, args...)
	}
}
