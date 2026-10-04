package tuning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Clone rehearsal: with a clone provider configured, an index is built
// for real on a disposable clone and the case statements are planned
// before and after. The monitored database is never touched, and the
// clone is always destroyed.

// Rehearser rehearses an index build.
type Rehearser interface {
	Rehearse(ctx context.Context, r RehearsalRequest) (RehearsalResult, error)
}

// RehearsalStatement is a statement to plan before and after.
type RehearsalStatement struct {
	QueryID int64
	Text    string
}

// RehearsalRequest is the index to build and the statements to plan.
type RehearsalRequest struct {
	DDL        string
	Statements []RehearsalStatement
}

// RehearsedQuery is one statement's plan cost before and after.
type RehearsedQuery struct {
	QueryID    int64   `json:"queryid,string"`
	BeforeCost float64 `json:"before_cost"`
	AfterCost  float64 `json:"after_cost"`
	Error      string  `json:"error,omitempty"`
}

// RehearsalResult is what the rehearsal measured.
type RehearsalResult struct {
	BuildMs        int64            `json:"build_ms"`
	SizeDeltaBytes int64            `json:"size_delta_bytes"`
	Queries        []RehearsedQuery `json:"queries"`
	Note           string           `json:"note,omitempty"`
}

// RehearsalOptions bound a rehearsal.
type RehearsalOptions struct {
	MaxCloneAge      time.Duration
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

// Rehearsal errors.
var (
	ErrCloneStale        = errors.New("tuning: the clone snapshot is too old to rehearse on")
	ErrCloneUnavailable  = errors.New("tuning: no clone is available")
	ErrRehearsalRefused  = errors.New("tuning: only one CREATE INDEX statement is rehearsed")
	errNoGenericPlanning = errors.New("a parameterized statement is planned only on " +
		"PostgreSQL 16 or later")
)

type cloneRehearser struct {
	provider clone.Provider
	opts     RehearsalOptions
}

// NewCloneRehearser returns a Rehearser on clones of provider.
func NewCloneRehearser(p clone.Provider, opts RehearsalOptions) Rehearser {
	return &cloneRehearser{provider: p, opts: opts}
}

func (r *cloneRehearser) Rehearse(ctx context.Context, req RehearsalRequest) (
	RehearsalResult, error) {
	ddl, ok := singleStatement(req.DDL)
	spec, err := optimizer.ParseIndexDDL(ddl)
	if !ok || err != nil || spec.TableSchema == "" {
		return RehearsalResult{}, ErrRehearsalRefused
	}
	age, err := r.provider.SnapshotAge(ctx)
	if err != nil {
		return RehearsalResult{}, fmt.Errorf("%w: snapshot age: %w", ErrCloneUnavailable, err)
	}
	if r.opts.MaxCloneAge > 0 && age > r.opts.MaxCloneAge {
		return RehearsalResult{}, fmt.Errorf("%w: %s old, limit %s", ErrCloneStale, age,
			r.opts.MaxCloneAge)
	}
	cl, err := r.provider.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		return RehearsalResult{}, fmt.Errorf("%w: %w", ErrCloneUnavailable, err)
	}
	res, err := r.onClone(ctx, cl, ddl, qualified(spec.TableSchema, spec.TableName),
		req.Statements)
	if derr := r.provider.Destroy(context.WithoutCancel(ctx), cl); derr != nil {
		err = errors.Join(err, fmt.Errorf("destroy clone %s: %w", cl.ID, derr))
	}
	return res, err
}

func (r *cloneRehearser) onClone(ctx context.Context, cl clone.Clone, ddl, table string,
	stmts []RehearsalStatement) (RehearsalResult, error) {
	conn, err := pgx.Connect(ctx, cl.DSN)
	if err != nil {
		return RehearsalResult{}, fmt.Errorf("%w: connect: %w", ErrCloneUnavailable, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if err := r.limit(ctx, conn); err != nil {
		return RehearsalResult{}, err
	}
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		return RehearsalResult{}, fmt.Errorf("clone version: %w", err)
	}
	res := RehearsalResult{Queries: make([]RehearsedQuery, len(stmts))}
	for i, s := range stmts {
		res.Queries[i].QueryID = s.QueryID
		res.Queries[i].BeforeCost, err = planCost(ctx, conn, s.Text, version)
		if err != nil {
			res.Queries[i].Error = err.Error()
		}
	}
	before, err := indexesSize(ctx, conn, table)
	if err != nil {
		return RehearsalResult{}, err
	}
	start := time.Now()
	if _, err := conn.Exec(ctx, ddl); err != nil {
		return RehearsalResult{}, fmt.Errorf("build the index on the clone: %w", err)
	}
	res.BuildMs = max(time.Since(start).Milliseconds(), 1)
	after, err := indexesSize(ctx, conn, table)
	if err != nil {
		return RehearsalResult{}, err
	}
	res.SizeDeltaBytes = after - before
	r.after(ctx, conn, stmts, version, &res)
	return res, nil
}

func (r *cloneRehearser) after(ctx context.Context, conn *pgx.Conn,
	stmts []RehearsalStatement, version int, res *RehearsalResult) {
	for i, s := range stmts {
		if res.Queries[i].Error != "" {
			continue
		}
		cost, err := planCost(ctx, conn, s.Text, version)
		if err != nil {
			res.Queries[i].Error = err.Error()
			continue
		}
		res.Queries[i].AfterCost = cost
	}
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

// planCost is the total cost of a statement's plan (EXPLAIN, never
// ANALYZE; a generic plan for a parameterized statement on PG16+).
func planCost(ctx context.Context, conn *pgx.Conn, text string, version int) (float64,
	error) {
	body, ok := singleStatement(text)
	if !ok {
		return 0, errMultiStatement
	}
	explain := "EXPLAIN (FORMAT JSON) "
	if paramRef.MatchString(blankLiteralsAndComments(body)) {
		if version < 160000 {
			return 0, errNoGenericPlanning
		}
		explain = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	raw, err := rawSingleValue(ctx, conn.PgConn(), explain+body)
	if err != nil {
		return 0, err
	}
	var plans []struct {
		Plan struct {
			TotalCost float64 `json:"Total Cost"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) == 0 {
		return 0, fmt.Errorf("unreadable plan: %v", err)
	}
	return plans[0].Plan.TotalCost, nil
}
