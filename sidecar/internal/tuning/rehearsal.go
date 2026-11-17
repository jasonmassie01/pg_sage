package tuning

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// Clone rehearsal: with a clone provider configured, an index is built
// for real on a disposable clone and the case statements are measured
// before and after: executed (EXPLAIN ANALYZE, BUFFERS) when the analyze
// guard proves them read-only and they have no parameters, planned
// otherwise. The clone is connected only after clone.GuardedConnect shows
// it is not production, the monitored database is never touched, and the
// clone is always destroyed (lab-04a).

// Rehearser rehearses an index build.
type Rehearser interface {
	Rehearse(ctx context.Context, r RehearsalRequest) (RehearsalResult, error)
}

// RehearsalStatement is a statement to measure before and after.
type RehearsalStatement struct {
	QueryID int64
	Text    string
}

// RehearsalRequest is the index to build and the statements to measure.
type RehearsalRequest struct {
	DDL        string
	Statements []RehearsalStatement
}

// RehearsedQuery is one statement's plan cost before and after, and the
// shared blocks (hit plus read) of one execution when it was executed.
type RehearsedQuery struct {
	QueryID      int64   `json:"queryid,string"`
	BeforeCost   float64 `json:"before_cost"`
	AfterCost    float64 `json:"after_cost"`
	BeforeBlocks *int64  `json:"before_blocks,omitempty"`
	AfterBlocks  *int64  `json:"after_blocks,omitempty"`
	// NotExecuted says why the statement was planned, not executed.
	NotExecuted string `json:"not_executed,omitempty"`
	Error       string `json:"error,omitempty"`
}

// RehearsalResult is what the rehearsal measured, the clone receipt and
// the provider.
type RehearsalResult struct {
	BuildMs        int64            `json:"build_ms"`
	SizeDeltaBytes int64            `json:"size_delta_bytes"`
	Queries        []RehearsedQuery `json:"queries"`
	Note           string           `json:"note,omitempty"`
	ReceiptID      int64            `json:"receipt_id,omitempty"`
	Provider       string           `json:"provider,omitempty"`
}

// RehearsalOptions bound a rehearsal. StatementTimeout and LockTimeout
// bound the clone session (the index build included); AnalyzeTimeout
// bounds each executed statement (0: DefaultAnalyzeTimeout).
type RehearsalOptions struct {
	MaxCloneAge      time.Duration
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	AnalyzeTimeout   time.Duration
	// Production identifies the server a clone must not be; without it no
	// clone is connected (clone.ErrTargetUnverified).
	Production clone.IdentitySource
	// Provider names the clone provider in the result (dle,
	// lakebase_branch).
	Provider string
	// Inspect is the analyze guard's parser; nil: libpg_query, absent in
	// a build without cgo, which then executes nothing.
	Inspect func(string) (sqlast.ReadQuery, error)
}

// DefaultAnalyzeTimeout is clone.rehearsal_statement_timeout_seconds'
// default: the longest one executed statement may run on the clone.
const DefaultAnalyzeTimeout = 5 * time.Second

// Rehearsal errors.
var (
	ErrCloneStale        = errors.New("tuning: the clone snapshot is too old to rehearse on")
	ErrCloneUnavailable  = errors.New("tuning: no clone is available")
	ErrRehearsalRefused  = errors.New("tuning: only one CREATE INDEX statement is rehearsed")
	ErrNoStatements      = errors.New("tuning: no target statement to rehearse")
	errNoGenericPlanning = errors.New("a parameterized statement is planned only on " +
		"PostgreSQL 16 or later")
)

type cloneRehearser struct {
	provider clone.Provider
	opts     RehearsalOptions
}

// NewCloneRehearser returns a Rehearser on clones of provider.
func NewCloneRehearser(p clone.Provider, opts RehearsalOptions) Rehearser {
	if opts.AnalyzeTimeout <= 0 {
		opts.AnalyzeTimeout = DefaultAnalyzeTimeout
	}
	return &cloneRehearser{provider: p, opts: opts}
}

func (r *cloneRehearser) Rehearse(ctx context.Context, req RehearsalRequest) (
	RehearsalResult, error) {
	ddl, ok := singleStatement(req.DDL)
	spec, err := optimizer.ParseIndexDDL(ddl)
	if !ok || err != nil || spec.TableSchema == "" {
		return RehearsalResult{}, ErrRehearsalRefused
	}
	if len(req.Statements) == 0 {
		return RehearsalResult{}, ErrNoStatements
	}
	if err := r.fresh(ctx); err != nil {
		return RehearsalResult{}, err
	}
	cl, err := r.provider.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		return RehearsalResult{}, fmt.Errorf("%w: %w", ErrCloneUnavailable, err)
	}
	receipt := r.receiptOf(cl)
	res, err := r.onClone(ctx, cl, ddl, sqlident.Qualified(spec.TableSchema, spec.TableName),
		req.Statements)
	res.ReceiptID, res.Provider = receipt, r.opts.Provider
	if derr := r.provider.Destroy(context.WithoutCancel(ctx), cl); derr != nil {
		err = errors.Join(err, fmt.Errorf("destroy clone %s: %w", cl.ID, derr))
	}
	return res, err
}

// fresh refuses a snapshot older than MaxCloneAge.
func (r *cloneRehearser) fresh(ctx context.Context) error {
	age, err := r.provider.SnapshotAge(ctx)
	if err != nil {
		return fmt.Errorf("%w: snapshot age: %w", ErrCloneUnavailable, err)
	}
	if r.opts.MaxCloneAge > 0 && age > r.opts.MaxCloneAge {
		return fmt.Errorf("%w: %s old, limit %s", ErrCloneStale, age, r.opts.MaxCloneAge)
	}
	return nil
}

func (r *cloneRehearser) receiptOf(cl clone.Clone) int64 {
	if namer, ok := r.provider.(clone.ReceiptNamer); ok {
		if id, ok := namer.ReceiptOf(cl.ID); ok {
			return id
		}
	}
	return 0
}

func (r *cloneRehearser) onClone(ctx context.Context, cl clone.Clone, ddl, table string,
	stmts []RehearsalStatement) (RehearsalResult, error) {
	pool, err := clone.GuardedConnect(ctx, cl, r.opts.Production)
	if err != nil {
		return RehearsalResult{}, err
	}
	defer pool.Close()
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return RehearsalResult{}, fmt.Errorf("%w: connect: %w", ErrCloneUnavailable, err)
	}
	defer pc.Release()
	conn := pc.Conn()
	if err := r.limit(ctx, conn); err != nil {
		return RehearsalResult{}, err
	}
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		return RehearsalResult{}, fmt.Errorf("clone version: %w", err)
	}
	m := measurer{conn: conn, version: version, opts: r.opts}
	res := RehearsalResult{Queries: m.before(ctx, stmts)}
	if err := build(ctx, conn, ddl, table, &res); err != nil {
		return RehearsalResult{}, err
	}
	m.after(ctx, stmts, res.Queries)
	return res, nil
}

// build creates the index on the clone and records its time and size.
func build(ctx context.Context, conn *pgx.Conn, ddl, table string,
	res *RehearsalResult) error {
	before, err := indexesSize(ctx, conn, table)
	if err != nil {
		return err
	}
	start := time.Now()
	if _, err := conn.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("build the index on the clone: %w", err)
	}
	res.BuildMs = max(time.Since(start).Milliseconds(), 1)
	after, err := indexesSize(ctx, conn, table)
	if err != nil {
		return err
	}
	res.SizeDeltaBytes = after - before
	return nil
}

// limit applies the statement and lock timeouts to the clone session.
func (r *cloneRehearser) limit(ctx context.Context, conn *pgx.Conn) error {
	for name, d := range map[string]time.Duration{"statement_timeout": r.opts.StatementTimeout,
		"lock_timeout": r.opts.LockTimeout} {
		if d <= 0 {
			continue
		}
		if _, err := conn.Exec(ctx, "SELECT set_config($1, $2, false)", name,
			fmt.Sprintf("%dms", d.Milliseconds())); err != nil {
			return fmt.Errorf("set %s on the clone: %w", name, err)
		}
	}
	return nil
}

func indexesSize(ctx context.Context, conn *pgx.Conn, table string) (int64, error) {
	var n int64
	if err := conn.QueryRow(ctx, "SELECT pg_indexes_size($1::regclass)", table).
		Scan(&n); err != nil {
		return 0, fmt.Errorf("index size of %s on the clone: %w", table, err)
	}
	return n, nil
}
