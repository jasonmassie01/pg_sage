package probes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// Limiter is a counting semaphore bounding concurrent probes.
type Limiter struct{ slots chan struct{} }

// NewLimiter returns a limiter of n slots, clamped to [1, MaxSidecarConcurrency].
func NewLimiter(n int) *Limiter {
	if n < 1 {
		n = 1
	}
	if n > MaxSidecarConcurrency {
		n = MaxSidecarConcurrency
	}
	return &Limiter{slots: make(chan struct{}, n)}
}

// Size is the number of slots.
func (l *Limiter) Size() int { return cap(l.slots) }

func (l *Limiter) acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Limiter) release() { <-l.slots }

// Runner executes catalog probes against one database: at most one probe
// at a time on this database, and at most the shared limiter's size
// across the sidecar.
type Runner struct {
	pool   *pgxpool.Pool
	reg    *Registry
	global *Limiter
	local  *Limiter
	// acquireWait bounds the pool wait of one attempt (tests shorten it).
	acquireWait time.Duration
	// afterAttempt, when set, sees every attempt's result (tests).
	afterAttempt func(attempt int, res Result)

	versionMu sync.Mutex
	version   int
}

// NewRunner binds a registry to a database pool. global is shared by
// every runner of the sidecar; nil gives this runner its own.
func NewRunner(pool *pgxpool.Pool, reg *Registry, global *Limiter) *Runner {
	if global == nil {
		global = NewLimiter(MaxSidecarConcurrency)
	}
	return &Runner{pool: pool, reg: reg, global: global, local: NewLimiter(1),
		acquireWait: acquireWait}
}

// Run executes one probe and returns its typed result. It never panics
// and never returns an untyped failure.
func (r *Runner) Run(ctx context.Context, id ID, args Args) Result {
	return r.run(ctx, id, args, false)
}

// RunBackground executes one probe for background sampling (the runway
// monitor): with the spec's BackgroundTimeout as its statement budget
// when it declares one, else exactly like Run. Investigations never use
// it.
func (r *Runner) RunBackground(ctx context.Context, id ID, args Args) Result {
	return r.run(ctx, id, args, true)
}

func (r *Runner) run(ctx context.Context, id ID, args Args, background bool) Result {
	res := Result{ProbeID: id, ObservedAt: time.Now()}
	if r == nil || r.pool == nil || r.reg == nil {
		return failed(res, StatusError, "not_configured", nil)
	}
	spec, ok := r.reg.Spec(id)
	if !ok {
		return failed(res, StatusError, "unknown_probe", nil)
	}
	if background && spec.BackgroundTimeout > 0 {
		spec.StatementTimeout = spec.BackgroundTimeout
	}
	res.Version = spec.Version
	if err := args.validate(spec.Args); err != nil {
		return failed(res, StatusError, "invalid_args", err)
	}
	queued := time.Now()
	release, err := r.acquire(ctx)
	res.Timing.Queue = time.Since(queued)
	if err != nil {
		return failedAt(res, PhaseQueue, StatusError, "concurrency_limit", err)
	}
	defer release()
	return r.attempts(ctx, spec, args, res)
}

// attempts runs the probe, once more after a server-side timeout when
// the retry budget and the caller's deadline allow. Timing sums the
// attempts; ElapsedMS is the execution time alone.
func (r *Runner) attempts(ctx context.Context, spec Spec, args Args, res Result) Result {
	for attempt := 1; ; attempt++ {
		out := r.attempt(ctx, spec, args, res)
		out.Timing.Attempts = attempt
		out.ElapsedMS = out.Timing.Execution.Milliseconds()
		if r.afterAttempt != nil {
			r.afterAttempt(attempt, out)
		}
		if out.Status.Usable() || !retryable(out.Reason, out.Phase) ||
			!retryAllowed(ctx, spec, attempt) {
			return out
		}
		res.Timing = out.Timing
		if sleepCtx(ctx, retryBackoff()) != nil {
			return out
		}
	}
}

// attempt takes a pool connection within acquireWait, then runs the
// probe on it within the statement budget: the pool wait never spends
// the statement's time.
func (r *Runner) attempt(ctx context.Context, spec Spec, args Args, res Result) Result {
	wait := r.acquireWait
	if wait <= 0 {
		wait = acquireWait
	}
	actx, cancel := context.WithTimeout(ctx, wait)
	began := time.Now()
	pool, store := r.poolFor(spec)
	conn, err := pool.Acquire(actx)
	res.Timing.Acquire += time.Since(began)
	cancel()
	if err != nil {
		st, reason := classify(err)
		return failedAt(res, PhasePoolAcquire, st, reason, err)
	}
	defer conn.Release()
	start := time.Now()
	res.ObservedAt = start
	res = r.runSpec(ctx, conn, spec, args, res, store)
	res.Timing.Execution += time.Since(start)
	return res
}

// acquire takes the per-database slot, then a sidecar slot, waiting at
// most queueWait.
func (r *Runner) acquire(ctx context.Context) (func(), error) {
	wctx, cancel := context.WithTimeout(ctx, queueWait)
	defer cancel()
	if err := r.local.acquire(wctx); err != nil {
		return nil, err
	}
	if err := r.global.acquire(wctx); err != nil {
		r.local.release()
		return nil, err
	}
	return func() { r.global.release(); r.local.release() }, nil
}

// poolFor is the pool a probe runs on and the store binding its SQL: a
// history probe runs on the database's history store (histstore.Resolve
// of the monitored pool), every other probe on the monitored database.
func (r *Runner) poolFor(spec Spec) (*pgxpool.Pool, histstore.Store) {
	if !spec.History {
		return r.pool, histstore.NewMonitored(r.pool)
	}
	st := histstore.Resolve(r.pool)
	if p, ok := st.DB().(*pgxpool.Pool); ok && st.Scoped() && p != nil {
		return p, st
	}
	return r.pool, st
}

func (r *Runner) runSpec(ctx context.Context, conn *pgxpool.Conn, spec Spec, args Args,
	res Result, store histstore.Store) Result {
	version, err := r.serverVersion(ctx, conn, store.Scoped())
	if err != nil {
		st, reason := classify(err)
		return failedAt(res, PhaseExecution, st, reason, err)
	}
	variant, ok := spec.VariantFor(version)
	if !ok {
		return failedAt(res, PhaseExecution, StatusUnsupported, "pg_version",
			fmt.Errorf("server_version_num %d", version))
	}
	res, err = r.execute(ctx, conn, spec, variant, args, res, store)
	if err != nil {
		st, reason := classify(err)
		return failedAt(res, PhaseExecution, st, reason, err)
	}
	if len(res.Rows) == 0 {
		res.Status, res.Reason = StatusEmpty, "no_rows"
		return res
	}
	res.Status = StatusOK
	return res
}

// serverVersion reads the server version once, on the probe's own
// connection: a busy pool at startup does not spend its budget. The
// history store's server (store) is another server: read each time.
func (r *Runner) serverVersion(ctx context.Context, conn *pgxpool.Conn, store bool) (int,
	error) {
	r.versionMu.Lock()
	defer r.versionMu.Unlock()
	if r.version > 0 && !store {
		return r.version, nil
	}
	vctx, cancel := context.WithTimeout(ctx, MaxStatementTimeout+clientMargin)
	defer cancel()
	var raw string
	if err := conn.QueryRow(vctx, "SHOW server_version_num").Scan(&raw); err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("server_version_num %q: %w", raw, err)
	}
	if !store {
		r.version = v
	}
	return v, nil
}

// sessionSettingsSQL fixes the probe transaction's settings. JIT is off:
// compiling a short catalog query costs more than it saves (lifeos:
// 10.6 ms of a 187 ms sequence_runway).
const sessionSettingsSQL = `SELECT
    pg_catalog.set_config('statement_timeout', $1, true),
    pg_catalog.set_config('lock_timeout', $2, true),
    pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true),
    pg_catalog.set_config('jit', 'off', true)`

// execute runs the probe in a read-only transaction whose settings are
// local to it, reading at most MaxRows rows and MaxBytes of payload.
func (r *Runner) execute(
	ctx context.Context, conn *pgxpool.Conn, spec Spec, v Variant, args Args, res Result,
	store histstore.Store,
) (Result, error) {
	qctx, cancel := context.WithTimeout(ctx, spec.StatementTimeout+clientMargin)
	defer cancel()
	tx, err := conn.BeginTx(qctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(qctx, sessionSettingsSQL, millis(spec.StatementTimeout),
		millis(spec.LockTimeout)); err != nil {
		return res, err
	}
	if err := checkRoles(qctx, tx, spec.Requires); err != nil {
		return res, err
	}
	sql, err := resolveExtension(qctx, tx, spec.Extension, v.SQL)
	if err != nil {
		return res, err
	}
	sql, params := store.Bind(sql, args.params(spec.Args, spec.MaxRows+1)...)
	rows, err := tx.Query(qctx, sql, params...)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	return collect(rows, spec, res)
}

// missingRoleSQL returns the first required role the session cannot use.
const missingRoleSQL = `SELECT r FROM pg_catalog.unnest($1::text[]) AS r
WHERE NOT pg_catalog.pg_has_role(current_user, r, 'USAGE') LIMIT 1`

// MissingRoleError is a probe that cannot see what it reads without a
// predefined role.
type MissingRoleError struct{ Role string }

func (e *MissingRoleError) Error() string {
	return fmt.Sprintf("role %s (granted by pg_monitor) is required to see other "+
		"roles' sessions", e.Role)
}

// checkRoles fails with a *MissingRoleError when the session lacks a
// required role.
func checkRoles(ctx context.Context, tx pgx.Tx, roles []string) error {
	if len(roles) == 0 {
		return nil
	}
	var missing string
	err := tx.QueryRow(ctx, missingRoleSQL, roles).Scan(&missing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return &MissingRoleError{Role: missing}
}

func millis(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }

func collect(rows pgx.Rows, spec Spec, res Result) (Result, error) {
	for _, fd := range rows.FieldDescriptions() {
		res.Columns = append(res.Columns, fd.Name)
	}
	envelope, err := json.Marshal(res)
	if err != nil {
		return res, err
	}
	size := len(envelope) + 64
	for rows.Next() {
		if len(res.Rows) == spec.MaxRows {
			res.Truncated = true
			break
		}
		row, n, err := readRow(rows, res.Columns)
		if err != nil {
			return res, err
		}
		if size+n+1 > spec.MaxBytes {
			res.Truncated = true
			break
		}
		size += n + 1
		res.Rows = append(res.Rows, row)
	}
	rows.Close()
	return res, rows.Err()
}

func readRow(rows pgx.Rows, cols []string) (Row, int, error) {
	vals, err := rows.Values()
	if err != nil {
		return nil, 0, err
	}
	row := make(Row, len(cols))
	for i, c := range cols {
		row[c] = normalize(vals[i])
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return nil, 0, err
	}
	return row, len(raw), nil
}

// normalize maps driver values to the Row contract: int64, float64,
// string, bool, time.Time (UTC) or nil.
func normalize(v any) any {
	switch x := v.(type) {
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float32:
		return float64(x)
	case time.Time:
		return x.UTC()
	case netip.Addr:
		return x.String()
	case netip.Prefix:
		return x.Addr().String()
	case pgtype.Numeric:
		f, err := x.Float64Value()
		if err != nil || !f.Valid {
			return nil
		}
		return f.Float64
	default:
		return v
	}
}

// failed marks res as not observed, dropping any partial rows so a
// failure is never read as a (partial) healthy answer.
func failed(res Result, st Status, reason string, err error) Result {
	res.Status, res.Reason = st, reason
	res.Rows, res.Truncated = nil, false
	if err != nil {
		res.Error = truncate(err.Error(), 200)
	}
	return res
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
