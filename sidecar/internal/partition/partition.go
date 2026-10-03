// Package partition keeps pg_sage's append-only history tables
// (sage.query_store, sage.snapshots) range-partitioned by UTC day, so
// retention drops a whole day instead of deleting rows one batch at a
// time (no dead tuples, no vacuum, no index bloat, space returned to the
// operating system at once).
//
// Layout of a partitioned table T:
//
//	T_history   FROM (MINVALUE) TO (<cutover>)  rows written before T was partitioned
//	T_pYYYYMMDD FROM (<day>) TO (<day + 1>)     one per UTC day from the cutover on
//	T_default   DEFAULT                         rows dated past every daily partition
//
// Convert turns an existing plain table into this layout in place (the old
// table becomes T_history, so no row is copied); Ensure creates the daily
// partitions ahead of the writers; Drop and DropHistory remove old data
// under a short lock timeout so they never queue readers behind them.
package partition

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LockTimeout bounds how long creating or dropping a partition
// waits for its lock. These statements need a strong lock on the parent;
// waiting longer would queue every reader behind them.
const LockTimeout = 2 * time.Second

const day = 24 * time.Hour

// Table is a sage history table partitioned by UTC day on Column.
type Table struct {
	Name   string // relation in schema sage
	Column string // timestamptz partition key
	// Key is the primary key the partitioned table keeps, without Column
	// (which is appended: a partitioned table's unique keys must include
	// it). Nil drops the plain table's primary key.
	Key []string
}

var (
	// QueryStore has no primary key: no reader looks a sample up by id.
	QueryStore = Table{Name: "query_store", Column: "captured_at"}
	// Snapshots keeps (id, collected_at): delta rows look their base up
	// by id (sage.snapshot_data).
	Snapshots = Table{Name: "snapshots", Column: "collected_at", Key: []string{"id"}}
)

// HistoryTables are the sage tables partitioned by day.
func HistoryTables() []Table { return []Table{QueryStore, Snapshots} }

// DayStart is the start of t's UTC day.
func DayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// DayName is the name of the partition holding day's rows.
func (t Table) DayName(d time.Time) string {
	return t.Name + "_p" + DayStart(d).Format("20060102")
}

// HistoryName is the partition holding rows from before the cutover.
func (t Table) HistoryName() string { return t.Name + "_history" }

// DefaultName is the partition holding rows no other partition takes.
func (t Table) DefaultName() string { return t.Name + "_default" }

func (t Table) parseDay(name string) (time.Time, bool) {
	digits, ok := strings.CutPrefix(name, t.Name+"_p")
	if !ok || len(digits) != 8 {
		return time.Time{}, false
	}
	d, err := time.Parse("20060102", digits)
	if err != nil {
		return time.Time{}, false
	}
	return d, true
}

func (t Table) ident() string { return pgx.Identifier{"sage", t.Name}.Sanitize() }

func (t Table) regclass() string { return "sage." + t.Name }

func child(name string) string { return pgx.Identifier{"sage", name}.Sanitize() }

// Partition is one partition of a Table.
type Partition struct {
	Name    string
	History bool
	Default bool
	Lower   time.Time // zero for the history and default partitions
	Upper   time.Time // exclusive; zero for the default partition
}

// DB is a pool, connection or transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ErrNoTable reports a table that does not exist.
var ErrNoTable = errors.New("partition: table does not exist")

// Partitioned reports whether t is a partitioned table.
func Partitioned(ctx context.Context, db DB, t Table) (bool, error) {
	var kind *string
	err := db.QueryRow(ctx, `SELECT (SELECT relkind::text FROM pg_catalog.pg_class
		WHERE oid = pg_catalog.to_regclass($1))`, t.regclass()).Scan(&kind)
	if err != nil {
		return false, fmt.Errorf("partition: read the kind of %s: %w", t.regclass(), err)
	}
	if kind == nil {
		return false, fmt.Errorf("%w: %s", ErrNoTable, t.regclass())
	}
	return *kind == "p", nil
}

const listSQL = `SELECT c.relname::text, pg_catalog.pg_get_expr(c.relpartbound, c.oid)
FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = pg_catalog.to_regclass($1)`

// List returns t's partitions: history first, then the days in order,
// default last. Partitions pg_sage did not create are left out.
func List(ctx context.Context, db DB, t Table) ([]Partition, error) {
	rows, err := db.Query(ctx, listSQL, t.regclass())
	if err != nil {
		return nil, fmt.Errorf("partition: list %s: %w", t.regclass(), err)
	}
	defer rows.Close()
	var out []Partition
	for rows.Next() {
		var name, bound string
		if err := rows.Scan(&name, &bound); err != nil {
			return nil, fmt.Errorf("partition: scan %s partitions: %w", t.regclass(), err)
		}
		if p, ok, err := classify(t, name, bound); err != nil {
			return nil, err
		} else if ok {
			out = append(out, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("partition: list %s: %w", t.regclass(), err)
	}
	sort.Slice(out, func(i, j int) bool { return rank(out[i]).Before(rank(out[j])) })
	return out, nil
}

// rank orders history first and default last.
func rank(p Partition) time.Time {
	switch {
	case p.History:
		return time.Time{}
	case p.Default:
		return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return p.Lower
}

func classify(t Table, name, bound string) (Partition, bool, error) {
	switch {
	case name == t.DefaultName() || bound == "DEFAULT":
		return Partition{Name: name, Default: true}, name == t.DefaultName(), nil
	case name == t.HistoryName():
		upper, err := parseUpperBound(bound)
		if err != nil {
			return Partition{}, false, fmt.Errorf("partition: %s: %w", name, err)
		}
		return Partition{Name: name, History: true, Upper: upper}, true, nil
	}
	d, ok := t.parseDay(name)
	if !ok {
		return Partition{}, false, nil
	}
	return Partition{Name: name, Lower: d, Upper: d.Add(day)}, true, nil
}

var upperBound = regexp.MustCompile(`^FOR VALUES FROM \(.*\) TO \('([^']+)'\)$`)

// parseUpperBound reads the exclusive upper bound of a range partition as
// pg_get_expr prints it (ISO DateStyle, the session's time zone).
func parseUpperBound(expr string) (time.Time, error) {
	m := upperBound.FindStringSubmatch(expr)
	if m == nil {
		return time.Time{}, fmt.Errorf("not a timestamp range bound: %q", expr)
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05.999999-07:00", "2006-01-02 15:04:05.999999-07:00:00"} {
		if ts, err := time.Parse(layout, m[1]); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unreadable bound %q", m[1])
}

// literal is a timestamptz literal for a partition bound.
func literal(ts time.Time) string {
	return "'" + ts.UTC().Format("2006-01-02 15:04:05") + "+00'"
}
