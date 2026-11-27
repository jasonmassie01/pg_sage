package executor

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A CREATE INDEX CONCURRENTLY reports its index's OID in
// pg_stat_progress_create_index as soon as the catalog entry is committed,
// right before it waits for the table's writers. A gate transaction that
// holds ROW EXCLUSIVE on the table (a writer, which blocks nobody) keeps
// the build in that wait until the OID has been read, so even a build that
// fails a millisecond later is bound to the object it registered.
const (
	buildPollFast   = 5 * time.Millisecond
	buildPollSlow   = 250 * time.Millisecond
	gateHoldDefault = 2 * time.Second
	gateHoldMax     = 5 * time.Second
)

// buildTarget is a CREATE INDEX CONCURRENTLY's table and index before it
// runs; absent records that the index name did not exist yet.
type buildTarget struct {
	TableOID int64
	Table    string
	Index    string
	Absent   bool
}

// createIndex runs a CREATE INDEX CONCURRENTLY, captures the OID it
// registers, and on failure drops that object only.
func (r remnantRun) createIndex(ctx context.Context, sql string) (*remnantCleanup, error) {
	target, err := resolveBuildTarget(ctx, r.pool, sql)
	if err != nil {
		buildErr := ExecConcurrently(ctx, r.pool, sql, r.timeout, r.opts...)
		if buildErr == nil {
			return nil, nil
		}
		cleanup := &remnantCleanup{Outcome: remnantUnbound, Reason: "the build was not " +
			"tracked (" + err.Error() + "); a remnant is left to the invalid_index finding"}
		r.log("remnant of a failed build not cleaned: %s", cleanup.summary())
		return cleanup, withCleanup(buildErr, cleanup)
	}
	watch := r.watchBuild(ctx, target)
	opts := append(append([]DDLOption{}, r.opts...), withBackendPID(watch.started))
	buildErr := ExecConcurrently(ctx, r.pool, sql, r.timeout, opts...)
	oid := watch.stop()
	if buildErr == nil {
		return nil, nil
	}
	cleanup := r.cleanBuild(ctx, target, oid, buildErr)
	r.log("cleanup after the failed build of %s: %s", target.Index, cleanup.summary())
	return cleanup, withCleanup(buildErr, cleanup)
}

// cleanBuild decides the failed build's cleanup.
func (r remnantRun) cleanBuild(
	ctx context.Context, target buildTarget, oid int64, buildErr error,
) *remnantCleanup {
	if skipped := skippedCleanup(buildErr); skipped != nil {
		return skipped
	}
	if !target.Absent {
		return &remnantCleanup{Outcome: remnantNothingToDrop,
			Reason: "the index name existed before the build; it is not the build's"}
	}
	if oid > 0 {
		return r.dropRemnants(ctx, []remnantCandidate{
			{Index: target.Index, OID: oid, TableOID: target.TableOID}})
	}
	var exists bool
	if err := r.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT to_regclass($1) IS NOT NULL`,
		target.Index).Scan(&exists); err == nil && !exists {
		return &remnantCleanup{Outcome: remnantNothingToDrop,
			Reason: "the build failed before it registered its index"}
	}
	return &remnantCleanup{Outcome: remnantUnbound, Reason: "the build's OID was not " +
		"captured; " + target.Index + " is left to the invalid_index finding"}
}

// resolveBuildTarget reads the build's schema-qualified table, the index
// it will create in that schema, and whether that name is free.
func resolveBuildTarget(ctx context.Context, pool *pgxpool.Pool, sql string) (
	buildTarget, error) {
	schemaName, tableName, _, _, ok := createIndexOn(sql)
	name := createIndexIdentifier(sql)
	switch {
	case !ok || schemaName == "":
		return buildTarget{}, fmt.Errorf("the build's table is not schema-qualified")
	case name == "":
		return buildTarget{}, fmt.Errorf("the build names no index")
	}
	var t buildTarget
	var nsp, rel string
	err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT c.oid::bigint, n.nspname, c.relname,
		to_regclass(quote_ident(n.nspname) || '.' || quote_ident($2)) IS NULL
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass($1)`,
		pgx.Identifier{schemaName, tableName}.Sanitize(), name).
		Scan(&t.TableOID, &nsp, &rel, &t.Absent)
	if err != nil {
		return buildTarget{}, fmt.Errorf("resolve the build's table: %w", err)
	}
	t.Table = pgx.Identifier{nsp, rel}.Sanitize()
	t.Index = pgx.Identifier{nsp, name}.Sanitize()
	return t, nil
}

// buildWatch captures the OID a running build registers.
type buildWatch struct {
	pid      chan uint32
	done     chan struct{}
	finished chan struct{}
	oid      atomic.Int64
}

func (r remnantRun) watchBuild(ctx context.Context, target buildTarget) *buildWatch {
	w := &buildWatch{pid: make(chan uint32, 1), done: make(chan struct{}),
		finished: make(chan struct{})}
	gate := holdBuildGate(ctx, r.pool, target.Table)
	go w.run(ctx, r, gate, gateHold(applyDDLOpts(r.opts).lockTimeoutMs))
	return w
}

func (w *buildWatch) started(pid uint32) {
	select {
	case w.pid <- pid:
	default:
	}
}

// stop ends the watch once the build returned and gives the captured OID
// (0 when none was seen).
func (w *buildWatch) stop() int64 {
	close(w.done)
	<-w.finished
	return w.oid.Load()
}

// run polls the build's progress row: fast, on the gate's own connection,
// while the gate holds the build before its first wait; slowly, on the
// pool, after. Polling the gate's connection keeps the watch from needing
// a third pool connection: on a two-connection pool (the default) the gate
// and the build hold both, and a pool poll would wait until the build
// timed out on the gate. The gate is released as soon as the OID is read,
// or after hold, so it can never time the build out.
func (w *buildWatch) run(ctx context.Context, r remnantRun, gate pgx.Tx, hold time.Duration) {
	defer close(w.finished)
	release := gateRelease(gate)
	defer release()
	var pid uint32
	select {
	case pid = <-w.pid:
	case <-w.done:
		return
	}
	readPool := func(c context.Context) int64 { return buildIndexOID(c, r.pool, pid) }
	read := readPool
	if gate != nil {
		read = func(c context.Context) int64 { return gateIndexOID(c, gate, pid) }
	}
	holdCtx, cancel := context.WithTimeout(ctx, hold)
	oid, finished := w.poll(holdCtx, read, buildPollFast)
	cancel()
	release()
	if oid == 0 && !finished {
		oid, _ = w.poll(ctx, readPool, buildPollSlow)
	}
	if oid > 0 {
		w.oid.Store(oid)
		if r.onCapture != nil {
			r.onCapture(oid)
		}
	}
}

// poll reads the build's OID every interval until it is seen (oid), the
// build has returned (finished) or ctx ends.
func (w *buildWatch) poll(ctx context.Context, read func(context.Context) int64,
	interval time.Duration) (oid int64, finished bool) {
	for {
		if oid := read(ctx); oid > 0 {
			return oid, false
		}
		select {
		case <-w.done:
			return 0, true
		case <-ctx.Done():
			return 0, false
		case <-time.After(interval):
		}
	}
}

const buildOIDQuery = `/* pg_sage executor */ SELECT COALESCE(max(index_relid::bigint), 0)
		FROM pg_stat_progress_create_index
		WHERE pid = $1 AND command = 'CREATE INDEX CONCURRENTLY'`

// buildIndexOID is the index a backend's CREATE INDEX CONCURRENTLY has
// registered, 0 before it has.
func buildIndexOID(ctx context.Context, pool *pgxpool.Pool, pid uint32) int64 {
	var oid int64
	if err := pool.QueryRow(ctx, buildOIDQuery, int64(pid)).Scan(&oid); err != nil {
		return 0
	}
	return oid
}

// gateIndexOID is buildIndexOID read inside the gate transaction. The
// progress views are snapshotted once per transaction, so the snapshot is
// cleared before each read.
func gateIndexOID(ctx context.Context, gate pgx.Tx, pid uint32) int64 {
	const clearSnapshot = `/* pg_sage executor */ SELECT pg_stat_clear_snapshot()`
	if _, err := gate.Exec(ctx, clearSnapshot); err != nil {
		return 0
	}
	var oid int64
	if err := gate.QueryRow(ctx, buildOIDQuery, int64(pid)).Scan(&oid); err != nil {
		return 0
	}
	return oid
}

// holdBuildGate opens the gate transaction; nil when the table lock is not
// free at once (the build then waits on that holder anyway, and is polled).
func holdBuildGate(ctx context.Context, pool *pgxpool.Pool, table string) pgx.Tx {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil
	}
	if _, err := tx.Exec(ctx, "/* pg_sage executor */ LOCK TABLE "+table+
		" IN ROW EXCLUSIVE MODE NOWAIT"); err != nil {
		gateRelease(tx)()
		return nil
	}
	return tx
}

// gateRelease ends the gate transaction once, however often it is called.
func gateRelease(gate pgx.Tx) func() {
	var once atomic.Bool
	return func() {
		if gate == nil || !once.CompareAndSwap(false, true) {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.Background(), remnantReleaseDeadline)
		defer cancel()
		_ = gate.Rollback(releaseCtx)
	}
}

// gateHold is how long the gate may hold the build: half its lock_timeout
// (at most gateHoldMax), so the build's own wait on the gate can never
// reach that timeout, however short it is.
func gateHold(lockTimeoutMS int) time.Duration {
	if lockTimeoutMS <= 0 {
		return gateHoldDefault
	}
	return min(time.Duration(lockTimeoutMS)*time.Millisecond/2, gateHoldMax)
}
