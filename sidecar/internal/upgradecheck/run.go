package upgradecheck

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Options tune one run.
type Options struct {
	// StatementTimeout bounds each statement; 0 is DefaultStatementTimeout.
	StatementTimeout time.Duration
	// TargetDB is a fleet member already at the target major (the request's
	// target_database); U2 reads its available extensions, read-only. nil
	// leaves U2 unknown.
	TargetDB *pgxpool.Pool
	// TargetDatabase names TargetDB in the report.
	TargetDatabase string
	// Now is the clock of U7; nil is time.Now.
	Now func() time.Time
	// inTx, when set (tests), runs inside the run's transaction after the
	// checks.
	inTx func(context.Context, pgx.Tx)
	// brief, when set (BuildBrief), reads the brief's facts inside the
	// run's transaction after the checks.
	brief func(context.Context, pgx.Tx, input)
}

func (o Options) timeout() time.Duration {
	if o.StatementTimeout <= 0 {
		return DefaultStatementTimeout
	}
	return o.StatementTimeout
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

// source is what every check knows about the server being upgraded.
type source struct {
	versionNum  int
	major       int
	database    string
	locProvider string // datlocprovider; "c" (libc) before PG15
}

// input is one check's context.
type input struct {
	src    source
	target Target
	opts   Options
	now    time.Time
}

// check is one of U1..U7.
type check struct {
	id, title string
	run       func(context.Context, pgx.Tx, input) (Check, error)
}

// checks is every check in report order.
func checks() []check {
	return []check{
		{"U1", "Tables without a primary key or replica identity", checkIdentity},
		{"U2", "Extensions available on the target", checkExtensions},
		{"U3", "Logical replication slots and subscriptions", checkReplication},
		{"U4", "Data checksums against the target's initdb default", checkChecksums},
		{"U5", "Collation versions and provider-dependent indexes", checkCollations},
		{"U6", "Settings the target removed or renamed", checkSettings},
		{"U7", "Support dates of the source and target majors", checkSupport},
	}
}

// Run checks pool's database for an upgrade to t in one READ ONLY
// REPEATABLE READ transaction with JIT off and a local statement timeout;
// each check runs under its own savepoint, so one that cannot read (for
// example permission denied) is unknown with its SQLSTATE and the others
// still run. It fails only when the transaction cannot be had, the target
// is invalid for the server, or ctx ends.
func Run(ctx context.Context, pool *pgxpool.Pool, t Target, opts Options) (Report, error) {
	if pool == nil {
		return Report{}, ErrNoPool
	}
	t = t.Normalized()
	if err := t.ValidateStatic(); err != nil {
		return Report{}, err
	}
	tx, err := catalogread.BeginSnapshot(ctx, pool, Tag, opts.timeout())
	if err != nil {
		return Report{}, fmt.Errorf("upgrade readiness: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // read-only
	src, err := readSource(ctx, tx)
	if err != nil {
		return Report{}, err
	}
	if err := t.Validate(src.major); err != nil {
		return Report{}, err
	}
	in := input{src: src, target: t, opts: opts, now: opts.now()}
	rep := Report{Database: src.database, SourceVersionNum: src.versionNum,
		SourceMajor: src.major, Target: t, TargetDatabase: opts.TargetDatabase,
		CheckedAt: in.now.UTC()}
	for _, c := range checks() {
		got, err := runOne(ctx, tx, c, in)
		if ctx.Err() != nil {
			return Report{}, fmt.Errorf("upgrade readiness: %w", ctx.Err())
		}
		if err != nil {
			got = unknownFrom(c, err)
		}
		rep.Checks = append(rep.Checks, got)
	}
	if opts.brief != nil {
		opts.brief(ctx, tx, in)
	}
	if opts.inTx != nil {
		opts.inTx(ctx, tx)
	}
	return rep, nil
}

func runOne(ctx context.Context, tx pgx.Tx, c check, in input) (Check, error) {
	var got Check
	err := catalogread.InSavepoint(ctx, tx, func(sp pgx.Tx) error {
		var err error
		got, err = c.run(ctx, sp, in)
		return err
	})
	if err != nil {
		return Check{}, err
	}
	got.ID, got.Title = c.id, c.title
	if got.Rows == nil {
		got.Rows = []Row{}
	}
	return got, nil
}

// unknownFrom is the verdict of a check that could not read.
func unknownFrom(c check, err error) Check {
	out := Check{ID: c.id, Title: c.title, Status: Unknown, Rows: []Row{},
		Reason: "pg_sage could not read this check's catalog data: " + err.Error()}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		out.SQLState = pgErr.Code
	}
	return out
}

var sourceSQL = Tag + `SELECT current_setting('server_version_num')::int,
  current_database()::text`

var locProviderSQL = Tag + `SELECT d.datlocprovider::text FROM pg_catalog.pg_database d
WHERE d.datname = current_database()`

func readSource(ctx context.Context, tx pgx.Tx) (source, error) {
	var s source
	if err := tx.QueryRow(ctx, sourceSQL).Scan(&s.versionNum, &s.database); err != nil {
		return source{}, fmt.Errorf("upgrade readiness: read the server version: %w", err)
	}
	s.major = s.versionNum / 10000
	s.locProvider = "c"
	if s.major >= 15 {
		if err := tx.QueryRow(ctx, locProviderSQL).Scan(&s.locProvider); err != nil {
			return source{}, fmt.Errorf("upgrade readiness: read the database's locale "+
				"provider: %w", err)
		}
	}
	return s, nil
}

// collect runs sql (its last column is count(*) OVER (), the rest text,
// $1 the row limit) and returns at most MaxRows rows named by cols and
// the total.
func collect(ctx context.Context, q pgx.Tx, cols []string, sql string,
	args ...any) ([]Row, int, error) {
	rows, err := q.Query(ctx, sql, append([]any{MaxRows}, args...)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Row{}
	total := 0
	for rows.Next() {
		vals := make([]*string, len(cols))
		dest := make([]any, 0, len(cols)+1)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var n int64
		if err := rows.Scan(append(dest, &n)...); err != nil {
			return nil, 0, err
		}
		total = int(n)
		row := Row{}
		for i, c := range cols {
			if vals[i] != nil {
				row[c] = *vals[i]
			} else {
				row[c] = ""
			}
		}
		out = append(out, row)
	}
	return out, total, rows.Err()
}
