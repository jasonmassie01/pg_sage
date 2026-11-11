package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The one handler for the INVALID remnant a failed CONCURRENTLY statement
// leaves behind (executor-05), on every path: autonomous, operator,
// replacement and restore. A remnant is dropped only when it is provably
// the object the failed statement registered: the OID captured from the
// build itself (CREATE INDEX CONCURRENTLY), or a _ccnew/_ccold name the
// failed REINDEX CONCURRENTLY added to its table. A VALID index is never
// dropped; anything unproven is left to the analyzer's invalid_index rule.

// remnantCleanupKey is the action-row before_state key of a cleanup.
const remnantCleanupKey = "remnant_cleanup"

// Per-index and overall cleanup outcomes.
const (
	remnantDropped         = "dropped"
	remnantNothingToDrop   = "nothing_to_drop"
	remnantGone            = "gone"
	remnantOIDMismatch     = "oid_mismatch"
	remnantValidKept       = "valid_kept"
	remnantInProgress      = "in_progress_elsewhere"
	remnantUnbound         = "unbound"
	remnantSkippedStopped  = "skipped_stopped"
	remnantCleanupFailed   = "remnant_cleanup_failed"
	remnantLockNamespace   = int64(0x52656d6e) // "Remn"
	remnantStateTimeout    = 5 * time.Second
	remnantReleaseDeadline = 2 * time.Second
)

// remnantCleanup is what one failed CONCURRENTLY statement's cleanup did.
type remnantCleanup struct {
	Outcome string         `json:"outcome"`
	Reason  string         `json:"reason,omitempty"`
	Indexes []remnantIndex `json:"indexes,omitempty"`
}

// remnantIndex is the outcome for one candidate remnant.
type remnantIndex struct {
	Name    string `json:"name"`
	OID     int64  `json:"oid"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// remnantCandidate is an index the failed statement registered: its
// qualified name, its OID and its table's OID.
type remnantCandidate struct {
	Index    string
	OID      int64
	TableOID int64
}

// remnantRun is what a CONCURRENTLY statement and its cleanup run with.
type remnantRun struct {
	pool        *pgxpool.Pool
	logFn       func(string, string, ...any)
	timeout     time.Duration
	dropTimeout time.Duration
	opts        []DDLOption
	// onCapture persists the OID of a build as soon as it is known (the
	// replacement records it, so a resume after a crash can bind to it).
	onCapture func(oid int64)
	// beforeDrop is a test seam run before each identity re-check.
	beforeDrop func(context.Context) error
}

func (r remnantRun) log(format string, args ...any) {
	if r.logFn != nil {
		r.logFn("executor", format, args...)
	}
}

// exec runs sql on the top-level path. A failed CREATE INDEX
// CONCURRENTLY or REINDEX CONCURRENTLY is cleaned up; the cleanup is nil
// when the statement succeeded or is neither.
func (r remnantRun) exec(ctx context.Context, sql string) (*remnantCleanup, error) {
	switch {
	case isConcurrentCreateIndex(sql):
		return r.createIndex(ctx, sql)
	case isConcurrentReindex(sql):
		return r.reindex(ctx, sql)
	}
	return nil, ExecConcurrently(ctx, r.pool, sql, r.timeout, r.opts...)
}

func isConcurrentCreateIndex(sql string) bool {
	return !IsIndexReplaceSQL(sql) && timeoutClassOf(sql) == classIndexBuild
}

func isConcurrentReindex(sql string) bool {
	return timeoutClassOf(sql) == classReindex
}

// skippedCleanup is the outcome when the failure itself rules a cleanup
// out: an emergency stop forbids more DDL.
func skippedCleanup(buildErr error) *remnantCleanup {
	var stopped *StoppedError
	if errors.As(buildErr, &stopped) {
		return &remnantCleanup{Outcome: remnantSkippedStopped,
			Reason: "an emergency stop ended the run; no cleanup DDL runs after it"}
	}
	return nil
}

// dropRemnants drops each candidate that is still provably the remnant.
func (r remnantRun) dropRemnants(
	ctx context.Context, candidates []remnantCandidate,
) *remnantCleanup {
	cleanup := &remnantCleanup{Outcome: remnantNothingToDrop}
	for _, c := range candidates {
		cleanup.Indexes = append(cleanup.Indexes, r.dropRemnant(ctx, c))
	}
	cleanup.Outcome = cleanup.overall()
	return cleanup
}

// dropRemnant re-checks the candidate's identity under an advisory lock
// on its OID (so two executors never both drop it), then drops it.
func (r remnantRun) dropRemnant(ctx context.Context, c remnantCandidate) remnantIndex {
	out := remnantIndex{Name: c.Index, OID: c.OID}
	if err := runHook(ctx, r.beforeDrop); err != nil {
		out.Outcome, out.Error = remnantCleanupFailed, err.Error()
		return out
	}
	release, held, err := lockRemnant(ctx, r.pool, c.OID)
	if err != nil {
		out.Outcome, out.Error = remnantCleanupFailed, err.Error()
		return out
	}
	defer release()
	if !held {
		out.Outcome = remnantInProgress
		return out
	}
	if out.Outcome, err = remnantVerdict(ctx, r.pool, c); err != nil || out.Outcome != "" {
		if err != nil {
			out.Outcome, out.Error = remnantCleanupFailed, err.Error()
		}
		return out
	}
	drop := "DROP INDEX CONCURRENTLY IF EXISTS " + c.Index
	if err := ExecConcurrently(ctx, r.pool, drop, r.dropTimeout, r.opts...); err != nil {
		out.Outcome, out.Error = remnantCleanupFailed, err.Error()
		return out
	}
	out.Outcome = remnantDropped
	return out
}

// lockRemnant takes a transaction advisory lock on the remnant's OID on a
// connection of its own; held is false when another executor holds it.
func lockRemnant(ctx context.Context, pool *pgxpool.Pool, oid int64) (func(), bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin remnant lock: %w", err)
	}
	release := func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), remnantReleaseDeadline)
		defer cancel()
		_ = tx.Rollback(releaseCtx)
	}
	var held bool
	if err := tx.QueryRow(ctx, `/* pg_sage executor */ SELECT pg_try_advisory_xact_lock($1)`,
		remnantLockNamespace<<32|oid).Scan(&held); err != nil {
		release()
		return nil, false, fmt.Errorf("take remnant lock on OID %d: %w", oid, err)
	}
	return release, held, nil
}

// remnantVerdict is why the candidate must be kept, or "" when it is still
// the INVALID index the failed statement registered on its table and no
// build is running on it.
func remnantVerdict(ctx context.Context, pool *pgxpool.Pool, c remnantCandidate) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, remnantStateTimeout)
	defer cancel()
	var oid, tableOID int64
	var valid, building bool
	err := pool.QueryRow(readCtx, `/* pg_sage executor */ SELECT
		COALESCE(i.indexrelid::bigint, 0), COALESCE(i.indisvalid, false),
		COALESCE(i.indrelid::bigint, 0), EXISTS (SELECT 1
		  FROM pg_stat_progress_create_index p WHERE p.index_relid = i.indexrelid)
		FROM (SELECT to_regclass($1) AS oid) r
		LEFT JOIN pg_index i ON i.indexrelid = r.oid`, c.Index).
		Scan(&oid, &valid, &tableOID, &building)
	switch {
	case err != nil:
		return "", fmt.Errorf("re-check remnant %s: %w", c.Index, err)
	case oid == 0:
		return remnantGone, nil
	case oid != c.OID || tableOID != c.TableOID:
		return remnantOIDMismatch, nil
	case valid:
		return remnantValidKept, nil
	case building:
		return remnantInProgress, nil
	}
	return "", nil
}

// overall folds the per-index outcomes: a failure first, then a drop,
// then the reason something was kept.
func (c *remnantCleanup) overall() string {
	for _, outcome := range []string{remnantCleanupFailed, remnantDropped,
		remnantOIDMismatch, remnantValidKept, remnantInProgress} {
		for _, idx := range c.Indexes {
			if idx.Outcome == outcome {
				return outcome
			}
		}
	}
	return remnantNothingToDrop
}

func (c *remnantCleanup) summary() string {
	parts := []string{c.Outcome}
	if c.Reason != "" {
		parts = append(parts, c.Reason)
	}
	for _, idx := range c.Indexes {
		line := fmt.Sprintf("%s (OID %d) %s", idx.Name, idx.OID, idx.Outcome)
		if idx.Error != "" {
			line += ": " + idx.Error
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "; ")
}

// withCleanup names the cleanup in the statement's error, keeping the
// error's identity (a lock timeout stays ErrLockNotAvailable).
func withCleanup(err error, c *remnantCleanup) error {
	if err == nil || c == nil {
		return err
	}
	return fmt.Errorf("%w (remnant_cleanup: %s)", err, c.summary())
}

// recordRemnantCleanup stores the cleanup in an action row's before_state.
// A retried statement records its latest cleanup, except that a cleanup
// which left a remnant behind is never replaced by a later one that did
// not (the retry then fails on the remnant's name, with nothing to drop).
func recordRemnantCleanup(state map[string]any, c *remnantCleanup) {
	if state == nil || c == nil {
		return
	}
	if prev, ok := state[remnantCleanupKey].(*remnantCleanup); ok &&
		prev.leftRemnant() && !c.leftRemnant() {
		return
	}
	state[remnantCleanupKey] = c
}

// leftRemnant reports a cleanup that may have left the statement's own
// INVALID index behind.
func (c *remnantCleanup) leftRemnant() bool {
	return c.Outcome == remnantCleanupFailed || c.Outcome == remnantUnbound
}
