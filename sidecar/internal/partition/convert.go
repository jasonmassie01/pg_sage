package partition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Conversion timeouts. Reading the old table's heap (VALIDATE CONSTRAINT,
// the pre-built key) happens under SHARE UPDATE EXCLUSIVE, which writers do
// not wait for, so it may take minutes; the ACCESS EXCLUSIVE transaction
// only changes the catalog.
const (
	convertStatementTimeout   = 10 * time.Minute
	exclusiveStatementTimeout = 30 * time.Second
	cleanupTimeout            = 30 * time.Second
	// maxAhead bounds how far past now the newest row may be dated: the
	// cutover moves past it, and a broken clock must not stretch the history
	// partition over months of future writes.
	maxAhead = 7 * day
)

var (
	// ErrBusy reports that another session is converting the table.
	ErrBusy = errors.New("partition: another session is converting the table")
	// ErrFutureRows reports rows dated more than a week ahead.
	ErrFutureRows = errors.New("partition: rows are dated more than a week ahead")
)

// convertHook runs between the conversion's phases (tests inject failures
// and measure): prepare, validate, index, exclusive, attach, committed.
var convertHook func(ctx context.Context, db DB, phase string) error

func runHook(ctx context.Context, db DB, phase string) error {
	if convertHook == nil {
		return nil
	}
	return convertHook(ctx, db, phase)
}

// Result is what one conversion did.
type Result struct {
	Converted bool
	Cut       time.Time     // the history partition's upper bound
	Validated time.Duration // VALIDATE CONSTRAINT (writers keep running)
	LockHeld  time.Duration // the ACCESS EXCLUSIVE transaction
}

// tableIndex is one index of the plain table being converted.
type tableIndex struct {
	name, def  string
	unique     bool
	primary    bool
	constraint string // backing constraint; "" for a plain index
}

// grant is one privilege on the plain table, re-granted on the new one.
type grant struct {
	grantee, privilege string
	grantable          bool
}

// convertPlan is what the exclusive transaction reads before changing
// anything.
type convertPlan struct {
	cutover   time.Time
	indexes   []tableIndex
	sequences map[string]string // column -> owned sequence
	grants    []grant
	owner     string
}

// checkName is the CHECK that proves the history partition's bound.
func (t Table) checkName() string { return t.Name + "_cutover_check" }

// keyName is the unique index pre-built for the partitioned primary key.
func (t Table) keyName() string { return t.Name + "_cutover_key" }

func (t Table) convertLockKey() string { return "sage.partition.convert." + t.Name }

// acquirer is a pool: Convert runs on one of its connections.
type acquirer interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// Convert turns the plain table t into a table partitioned by UTC day, in
// place: the old table becomes the history partition, so no row is copied
// and the table is not rewritten. It never reads the old heap under an
// exclusive lock:
//
//  1. ADD CONSTRAINT <col> < cut CHECK ... NOT VALID (a catalog change),
//     VALIDATE CONSTRAINT (one heap scan under SHARE UPDATE EXCLUSIVE:
//     writers keep running) and, for a Key, CREATE UNIQUE INDEX
//     CONCURRENTLY on Key plus the day column;
//  2. one ACCESS EXCLUSIVE transaction that only changes the catalog:
//     ATTACH finds its bound proven by the validated CHECK and the key
//     built, then the CHECK is dropped.
//
// The cut is the second UTC midnight after now (or after the newest row,
// at most a week ahead: ErrFutureRows otherwise), so rows written between
// VALIDATE and the exclusive lock satisfy the CHECK. Any failure leaves
// the plain table as it was: the CHECK and the pre-built key are removed
// (or by a later Cleanup when the lock to remove them is not available).
// Converted is false when t is partitioned already. db is a pool or one
// session (not a transaction); the session's timeouts are restored.
func Convert(ctx context.Context, db DB, t Table) (Result, error) {
	if done, err := Partitioned(ctx, db, t); err != nil || done {
		return Result{}, err
	}
	if p, ok := db.(acquirer); ok {
		conn, err := p.Acquire(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("partition: convert %s: %w", t.regclass(), err)
		}
		defer conn.Release()
		db = conn
	}
	return withConvertLock(ctx, db, t, func(s DB) (Result, error) {
		res, err := convertLocked(ctx, s, t)
		if err == nil {
			return res, nil
		}
		err = fmt.Errorf("partition: convert %s: %w", t.regclass(), err)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		// The failed step may have left any statement timeout on the session;
		// the undo and the unlock run under the cleanup's, and withConvertLock
		// restores the caller's afterwards.
		if _, rerr := setSession(cctx, s, LockTimeout, cleanupTimeout); rerr != nil {
			return res, errors.Join(err, fmt.Errorf("partition: undo the conversion of %s: %w",
				t.regclass(), rerr))
		}
		if cerr := cleanupLocked(cctx, s, t); cerr != nil {
			err = errors.Join(err, fmt.Errorf("partition: undo the conversion of %s: %w",
				t.regclass(), cerr))
		}
		return res, err
	})
}

// withConvertLock runs fn holding the session advisory lock that
// serializes conversions and cleanups of t, with the session's timeouts
// set for the conversion and restored after.
func withConvertLock(ctx context.Context, s DB, t Table,
	fn func(DB) (Result, error)) (res Result, err error) {
	restore, err := setSession(ctx, s, LockTimeout, convertStatementTimeout)
	if err != nil {
		return Result{}, fmt.Errorf("partition: convert %s: %w", t.regclass(), err)
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	defer func() { err = errors.Join(err, restore(bg)) }()
	var got bool
	if err := s.QueryRow(ctx, "SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtext($1))",
		t.convertLockKey()).Scan(&got); err != nil {
		return Result{}, fmt.Errorf("partition: convert %s: advisory lock: %w", t.regclass(), err)
	}
	if !got {
		return Result{}, fmt.Errorf("%w: %s", ErrBusy, t.regclass())
	}
	defer func() {
		_, uerr := s.Exec(bg, "SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtext($1))",
			t.convertLockKey())
		err = errors.Join(err, uerr)
	}()
	return fn(s)
}

// setSession sets the session's lock and statement timeouts and returns
// a func restoring the previous values.
func setSession(ctx context.Context, s DB, lock, stmt time.Duration) (
	func(context.Context) error, error) {
	// One statement reads the old values and sets the new ones: a session
	// left with a tiny statement timeout runs as little as possible under it.
	var oldLock, oldStmt string
	if err := s.QueryRow(ctx, `SELECT pg_catalog.current_setting('lock_timeout'),
		pg_catalog.current_setting('statement_timeout'),
		pg_catalog.set_config('lock_timeout', $1, false),
		pg_catalog.set_config('statement_timeout', $2, false)`, ms(lock), ms(stmt)).
		Scan(&oldLock, &oldStmt, nil, nil); err != nil {
		return nil, fmt.Errorf("set session timeouts: %w", err)
	}
	set := func(ctx context.Context, lock, stmt string) error {
		_, err := s.Exec(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, false),
			pg_catalog.set_config('statement_timeout', $2, false)`, lock, stmt)
		return err
	}
	return func(ctx context.Context) error {
		if err := set(ctx, oldLock, oldStmt); err != nil {
			return fmt.Errorf("restore session timeouts: %w", err)
		}
		return nil
	}, nil
}

func ms(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }

// convertLocked runs the conversion on session s, which holds the
// conversion lock.
func convertLocked(ctx context.Context, s DB, t Table) (Result, error) {
	var res Result
	if err := cleanupLocked(ctx, s, t); err != nil {
		return res, fmt.Errorf("remove an unfinished conversion: %w", err)
	}
	if done, err := Partitioned(ctx, s, t); err != nil || done {
		return res, err
	}
	cut, err := chooseCut(ctx, s, t)
	if err != nil {
		return res, err
	}
	res.Cut = cut
	if res.Validated, err = prepare(ctx, s, t, cut); err != nil {
		return res, err
	}
	if err := runHook(ctx, s, "exclusive"); err != nil {
		return res, err
	}
	if res.LockHeld, err = cutover(ctx, s, t, cut); err != nil {
		return res, err
	}
	res.Converted = true
	return res, runHook(ctx, s, "committed")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func readPlan(ctx context.Context, tx pgx.Tx, t Table, cut time.Time) (convertPlan, error) {
	p := convertPlan{cutover: cut, sequences: map[string]string{}}
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_get_userbyid(relowner)::text
		FROM pg_catalog.pg_class WHERE oid = pg_catalog.to_regclass($1)`, t.regclass()).
		Scan(&p.owner); err != nil {
		return p, fmt.Errorf("read owner: %w", err)
	}
	var err error
	if p.indexes, err = readIndexes(ctx, tx, t); err != nil {
		return p, err
	}
	if p.sequences, err = readSequences(ctx, tx, t); err != nil {
		return p, err
	}
	p.grants, err = readGrants(ctx, tx, t, p.owner)
	return p, err
}

// readIndexes reads the plain table's indexes but the pre-built key.
func readIndexes(ctx context.Context, tx pgx.Tx, t Table) ([]tableIndex, error) {
	rows, err := tx.Query(ctx, `SELECT c.relname::text, pg_catalog.pg_get_indexdef(i.indexrelid),
		i.indisunique, i.indisprimary, COALESCE(con.conname::text, '')
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		LEFT JOIN pg_catalog.pg_constraint con
		  ON con.conindid = i.indexrelid AND con.conrelid = i.indrelid
		WHERE i.indrelid = pg_catalog.to_regclass($1) AND c.relname <> $2
		ORDER BY c.relname`, t.regclass(), t.keyName())
	if err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tableIndex, error) {
		var ix tableIndex
		err := r.Scan(&ix.name, &ix.def, &ix.unique, &ix.primary, &ix.constraint)
		return ix, err
	})
	if err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	for _, ix := range out {
		if (ix.unique || ix.constraint != "") && !ix.primary {
			return nil, fmt.Errorf("unique index %s cannot carry over to a table "+
				"partitioned by %s", ix.name, t.Column)
		}
	}
	return out, nil
}

func readSequences(ctx context.Context, tx pgx.Tx, t Table) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT a.attname::text,
		pg_catalog.pg_get_serial_sequence($1, a.attname)
		FROM pg_catalog.pg_attribute a
		WHERE a.attrelid = pg_catalog.to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped
		  AND pg_catalog.pg_get_serial_sequence($1, a.attname) IS NOT NULL`, t.regclass())
	if err != nil {
		return nil, fmt.Errorf("read sequences: %w", err)
	}
	out := map[string]string{}
	for rows.Next() {
		var col, seq string
		if err := rows.Scan(&col, &seq); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read sequences: %w", err)
		}
		out[col] = seq
	}
	rows.Close()
	return out, rows.Err()
}

func readGrants(ctx context.Context, tx pgx.Tx, t Table, owner string) ([]grant, error) {
	rows, err := tx.Query(ctx, `SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC'
		ELSE pg_catalog.pg_get_userbyid(a.grantee)::text END, a.privilege_type, a.is_grantable
		FROM pg_catalog.pg_class c, pg_catalog.aclexplode(c.relacl) a
		WHERE c.oid = pg_catalog.to_regclass($1)`, t.regclass())
	if err != nil {
		return nil, fmt.Errorf("read grants: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (grant, error) {
		var g grant
		err := r.Scan(&g.grantee, &g.privilege, &g.grantable)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("read grants: %w", err)
	}
	out := all[:0]
	for _, g := range all {
		if g.grantee != owner {
			out = append(out, g)
		}
	}
	return out, nil
}
