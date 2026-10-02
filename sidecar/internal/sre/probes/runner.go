package probes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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

	versionMu sync.Mutex
	version   int
}

// NewRunner binds a registry to a database pool. global is shared by
// every runner of the sidecar; nil gives this runner its own.
func NewRunner(pool *pgxpool.Pool, reg *Registry, global *Limiter) *Runner {
	if global == nil {
		global = NewLimiter(MaxSidecarConcurrency)
	}
	return &Runner{pool: pool, reg: reg, global: global, local: NewLimiter(1)}
}

// Run executes one probe and returns its typed result. It never panics
// and never returns an untyped failure.
func (r *Runner) Run(ctx context.Context, id ID, args Args) Result {
	res := Result{ProbeID: id, ObservedAt: time.Now()}
	if r == nil || r.pool == nil || r.reg == nil {
		return failed(res, StatusError, "not_configured", nil)
	}
	spec, ok := r.reg.Spec(id)
	if !ok {
		return failed(res, StatusError, "unknown_probe", nil)
	}
	res.Version = spec.Version
	if err := args.validate(spec.Args); err != nil {
		return failed(res, StatusError, "invalid_args", err)
	}
	release, err := r.acquire(ctx)
	if err != nil {
		return failed(res, StatusError, "concurrency_limit", err)
	}
	defer release()
	start := time.Now()
	res.ObservedAt = start
	res = r.runSpec(ctx, spec, args, res)
	res.ElapsedMS = time.Since(start).Milliseconds()
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

func (r *Runner) runSpec(ctx context.Context, spec Spec, args Args, res Result) Result {
	version, err := r.serverVersion(ctx)
	if err != nil {
		st, reason := classify(err)
		return failed(res, st, reason, err)
	}
	variant, ok := spec.VariantFor(version)
	if !ok {
		return failed(res, StatusUnsupported, "pg_version",
			fmt.Errorf("server_version_num %d", version))
	}
	res, err = r.execute(ctx, spec, variant, args, res)
	if err != nil {
		st, reason := classify(err)
		return failed(res, st, reason, err)
	}
	if len(res.Rows) == 0 {
		res.Status, res.Reason = StatusEmpty, "no_rows"
		return res
	}
	res.Status = StatusOK
	return res
}

func (r *Runner) serverVersion(ctx context.Context) (int, error) {
	r.versionMu.Lock()
	defer r.versionMu.Unlock()
	if r.version > 0 {
		return r.version, nil
	}
	vctx, cancel := context.WithTimeout(ctx, MaxStatementTimeout)
	defer cancel()
	var raw string
	if err := r.pool.QueryRow(vctx, "SHOW server_version_num").Scan(&raw); err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("server_version_num %q: %w", raw, err)
	}
	r.version = v
	return v, nil
}

const sessionSettingsSQL = `SELECT
    pg_catalog.set_config('statement_timeout', $1, true),
    pg_catalog.set_config('lock_timeout', $2, true),
    pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true)`

// execute runs the probe in a read-only transaction whose settings are
// local to it, reading at most MaxRows rows and MaxBytes of payload.
func (r *Runner) execute(
	ctx context.Context, spec Spec, v Variant, args Args, res Result,
) (Result, error) {
	qctx, cancel := context.WithTimeout(ctx, spec.StatementTimeout+time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(qctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(qctx, sessionSettingsSQL, millis(spec.StatementTimeout),
		millis(spec.LockTimeout)); err != nil {
		return res, err
	}
	sql, err := resolveExtension(qctx, tx, spec.Extension, v.SQL)
	if err != nil {
		return res, err
	}
	rows, err := tx.Query(qctx, sql, args.params(spec.Args, spec.MaxRows+1)...)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	return collect(rows, spec, res)
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
