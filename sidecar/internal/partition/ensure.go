package partition

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ensure creates the missing daily partitions of t for the days days
// starting on from's UTC day, and returns how many it created. Days the
// history partition covers need none. A plain (not yet partitioned) table
// is left alone. Rows already waiting in the default partition for a day
// move into that day's new partition. Safe to call concurrently.
func Ensure(ctx context.Context, db DB, t Table, from time.Time, days int) (int, error) {
	if days < 1 {
		return 0, fmt.Errorf("partition: ensure %s: days must be positive, got %d",
			t.regclass(), days)
	}
	ok, err := Partitioned(ctx, db, t)
	if err != nil || !ok {
		return 0, err
	}
	parts, err := List(ctx, db, t)
	if err != nil {
		return 0, err
	}
	have, covered, hasDefault := map[string]bool{}, time.Time{}, false
	for _, p := range parts {
		have[p.Name] = true
		hasDefault = hasDefault || p.Default
		if p.History {
			covered = p.Upper
		}
	}
	created, end := 0, DayStart(from).Add(time.Duration(days)*day)
	for d := DayStart(from); d.Before(end); d = d.Add(day) {
		if d.Before(covered) || have[t.DayName(d)] {
			continue
		}
		made, err := createDay(ctx, db, t, d, hasDefault)
		if err != nil {
			return created, err
		}
		if made {
			created++
		}
	}
	return created, nil
}

// createDay creates and attaches one day's partition. Concurrent callers
// serialize on an advisory lock; the loser finds the partition there.
func createDay(ctx context.Context, db DB, t Table, d time.Time, hasDefault bool) (bool,
	error) {
	name := t.DayName(d)
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("partition: create %s: begin: %w", name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockedSession(ctx, tx, "sage.partition."+t.Name, LockTimeout); err != nil {
		return false, fmt.Errorf("partition: create %s: %w", name, err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1) IS NOT NULL`,
		"sage."+name).Scan(&exists); err != nil {
		return false, fmt.Errorf("partition: create %s: check: %w", name, err)
	}
	if exists {
		return false, nil
	}
	if err := attachDay(ctx, tx, t, name, d, hasDefault); err != nil {
		return false, fmt.Errorf("partition: create %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("partition: create %s: commit: %w", name, err)
	}
	return true, nil
}

// attachDay builds the day's table, moves the day's rows out of the
// default partition into it, and attaches it. Building it apart and
// attaching it takes SHARE UPDATE EXCLUSIVE on the parent, which readers
// and writers do not wait for (CREATE TABLE ... PARTITION OF would take
// ACCESS EXCLUSIVE).
func attachDay(ctx context.Context, tx pgx.Tx, t Table, name string, d time.Time,
	hasDefault bool) error {
	stmts := []string{fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS
		INCLUDING CONSTRAINTS INCLUDING STORAGE)`, child(name), t.ident())}
	if hasDefault {
		stmts = append(stmts, fmt.Sprintf(`WITH moved AS (DELETE FROM ONLY %s
			WHERE %s >= %s AND %s < %s RETURNING *) INSERT INTO %s SELECT * FROM moved`,
			child(t.DefaultName()), pgx.Identifier{t.Column}.Sanitize(), literal(d),
			pgx.Identifier{t.Column}.Sanitize(), literal(d.Add(day)), child(name)))
	}
	stmts = append(stmts, fmt.Sprintf(`ALTER TABLE %s ATTACH PARTITION %s
		FOR VALUES FROM (%s) TO (%s)`, t.ident(), child(name), literal(d), literal(d.Add(day))))
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// lockedSession bounds the transaction's lock waits and serializes it with
// other partition maintenance of the same table.
func lockedSession(ctx context.Context, tx pgx.Tx, key string, wait time.Duration) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'",
		wait.Milliseconds())); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(
		pg_catalog.hashtext($1))`, key); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	return nil
}

// Drop drops one of t's partitions, giving up after LockTimeout.
func Drop(ctx context.Context, db DB, t Table, p Partition) error {
	return onPartition(ctx, db, t, p, "DROP TABLE %s")
}

// Truncate empties one of t's partitions, giving up after LockTimeout.
func Truncate(ctx context.Context, db DB, t Table, p Partition) error {
	return onPartition(ctx, db, t, p, "TRUNCATE %s")
}

func onPartition(ctx context.Context, db DB, t Table, p Partition, stmt string) error {
	parts, err := List(ctx, db, t)
	if err != nil {
		return err
	}
	known := false
	for _, q := range parts {
		known = known || q.Name == p.Name
	}
	if !known {
		return fmt.Errorf("partition: %s is not a partition of %s", p.Name, t.regclass())
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("partition: %s: begin: %w", p.Name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockedSession(ctx, tx, "sage.partition."+t.Name, LockTimeout); err != nil {
		return fmt.Errorf("partition: %s: %w", p.Name, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(stmt, child(p.Name))); err != nil {
		return fmt.Errorf("partition: %s: %w", p.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("partition: %s: commit: %w", p.Name, err)
	}
	return nil
}

// Size is the total size of t (heap, TOAST and indexes) with every
// partition.
func Size(ctx context.Context, db DB, t Table) (int64, error) {
	var n *int64
	// pg_partition_tree lists nothing for a plain table.
	err := db.QueryRow(ctx, `SELECT CASE
		WHEN pg_catalog.to_regclass($1) IS NULL THEN NULL
		WHEN (SELECT relkind FROM pg_catalog.pg_class
		      WHERE oid = pg_catalog.to_regclass($1)) <> 'p'
		THEN pg_catalog.pg_total_relation_size(pg_catalog.to_regclass($1))
		ELSE (SELECT COALESCE(sum(pg_catalog.pg_total_relation_size(relid)), 0)::bigint
		      FROM pg_catalog.pg_partition_tree(pg_catalog.to_regclass($1))) END`,
		t.regclass()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("partition: size of %s: %w", t.regclass(), err)
	}
	if n == nil {
		return 0, fmt.Errorf("%w: %s", ErrNoTable, t.regclass())
	}
	return *n, nil
}

// Keeper remembers which days it has ensured, so writers can call Ensure
// before every write at the cost of a map lookup.
type Keeper struct {
	mu      sync.Mutex
	through map[string]time.Time // per table: days ensured up to (exclusive)
}

// NewKeeper returns a Keeper that has ensured nothing yet.
func NewKeeper() *Keeper { return &Keeper{through: map[string]time.Time{}} }

// Ensure makes sure at's day and the next exist for each table.
func (k *Keeper) Ensure(ctx context.Context, db DB, at time.Time, tables ...Table) error {
	if k == nil || db == nil {
		return errors.New("partition: keeper needs a database")
	}
	need := DayStart(at).Add(2 * day)
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, t := range tables {
		if !k.through[t.Name].Before(need) {
			continue
		}
		if _, err := Ensure(ctx, db, t, at, 2); err != nil {
			return err
		}
		k.through[t.Name] = need
	}
	return nil
}
