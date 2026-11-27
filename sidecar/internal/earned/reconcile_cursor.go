package earned

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The incident pass (earned-02) reads each kind, L2 handoffs and L3
// auto-executions, forward from a cursor kept in sage.trust_ledger_state:
// the largest action_log id read whose execution is older than
// incidentCursorLag, so a smaller id committing late (inside the lag) is
// still read. The ids at or below the cursor that still wait (no verdict
// yet, or their L3 notice failed) are its pending set, read again by id
// each pass until they are done or leave the lookback. Without a cursor
// (NULL: before the first pass) the read covers the whole lookback.

const (
	// incidentBatch bounds one kind's forward read per pass.
	incidentBatch = 1000
	// incidentPendingCap bounds one kind's pending set; the oldest ids
	// over it are dropped with a warning.
	incidentPendingCap = 1000
	// incidentCursorLag is how old an execution must be before the cursor
	// moves past it.
	incidentCursorLag = 5 * time.Minute
)

// incidentCursor is one kind's position: nil cursor reads the lookback.
type incidentCursor struct {
	cursor  *int64
	pending []int64
}

// incidentCursors are both kinds' positions.
type incidentCursors struct{ handoff, auto incidentCursor }

func (c incidentCursor) equal(o incidentCursor) bool {
	sameCursor := (c.cursor == nil) == (o.cursor == nil) &&
		(c.cursor == nil || *c.cursor == *o.cursor)
	return sameCursor && slices.Equal(c.pending, o.pending)
}

func (c incidentCursors) equal(o incidentCursors) bool {
	return c.handoff.equal(o.handoff) && c.auto.equal(o.auto)
}

// incidentKind is one kind's reads and row scanner.
type incidentKind struct {
	what             string
	forward, pending string
	scan             func(*Reconciler, pgx.Rows) (executed, bool, error)
}

var (
	handoffKind = incidentKind{what: "executed autonomy handoffs", forward: handoffSQL,
		pending: handoffPendingSQL, scan: (*Reconciler).scanHandoff}
	autoKind = incidentKind{what: "autonomous executions", forward: autoExecutionSQL,
		pending: autoExecutionPendingSQL, scan: (*Reconciler).scanAutoExecution}
)

// incidentRead is one kind's rows this pass, pending ones first, and the
// largest forward id older than the lag (nil: none).
type incidentRead struct {
	rows []executed
	aged *int64
}

// incidents reads both kinds from their cursors, notifies the L3
// executions, records every decided outcome and saves the cursors. It
// returns the joined notice errors apart from a failure of the pass; the
// cursors are saved either way, keeping a failed notice pending.
func (r *Reconciler) incidents(ctx context.Context, res *ReconcileResult) (error, error) {
	loaded, err := r.svc.store.incidentCursors(ctx)
	if err != nil {
		return nil, err
	}
	handoffs, err := r.readIncident(ctx, handoffKind, loaded.handoff, res)
	if err != nil {
		return nil, err
	}
	autos, err := r.readIncident(ctx, autoKind, loaded.auto, res)
	if err != nil {
		return nil, err
	}
	undone, notifyErr, err := r.notifyAll(ctx, autos.rows, res)
	if err != nil {
		return notifyErr, err
	}
	for _, x := range append(handoffs.rows, autos.rows...) {
		decided, err := r.record(ctx, x, res)
		if err != nil {
			return notifyErr, err
		}
		if !decided {
			undone[x.actionLogID] = true
		}
	}
	var next incidentCursors
	var overflow []int64
	next.handoff, overflow = nextIncidentCursor(loaded.handoff, handoffs.aged,
		handoffs.rows, undone)
	r.dropPending("handoff", overflow, "over the pending cap", res)
	next.auto, overflow = nextIncidentCursor(loaded.auto, autos.aged, autos.rows, undone)
	r.dropPending("auto-execution", overflow, "over the pending cap", res)
	if next.equal(loaded) {
		return notifyErr, nil
	}
	return notifyErr, r.svc.store.saveIncidentCursors(ctx, loaded, next)
}

// readIncident reads kind's pending ids again, then forward from its
// cursor. A pending id the lookback no longer returns is dropped.
func (r *Reconciler) readIncident(ctx context.Context, kind incidentKind,
	cur incidentCursor, res *ReconcileResult) (incidentRead, error) {
	var out incidentRead
	base := []any{r.lookback.Seconds(), UnverifiedAfter.Seconds(),
		incidentCursorLag.Seconds()}
	if len(cur.pending) > 0 {
		seen := map[int64]bool{}
		err := r.scanIncident(ctx, kind, kind.pending, append(base, cur.pending), res,
			func(x executed) { seen[x.actionLogID] = true }, &out)
		if err != nil {
			return out, err
		}
		var gone []int64
		for _, id := range cur.pending {
			if !seen[id] {
				gone = append(gone, id)
			}
		}
		r.dropPending(kind.what, gone, "older than the lookback or gone", res)
	}
	err := r.scanIncident(ctx, kind, kind.forward, append(base, cur.cursor, incidentBatch),
		res, func(x executed) {
			if x.aged && (out.aged == nil || x.actionLogID > *out.aged) {
				id := x.actionLogID
				out.aged = &id
			}
		}, &out)
	return out, err
}

// scanIncident runs one read and keeps its known rows in out; each row,
// skipped or not, is passed to seen first.
func (r *Reconciler) scanIncident(ctx context.Context, kind incidentKind, query string,
	args []any, res *ReconcileResult, seen func(executed), out *incidentRead) error {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read %s on %s: %w", kind.what, r.database, err)
	}
	defer rows.Close()
	for rows.Next() {
		x, ok, err := kind.scan(r, rows)
		if err != nil {
			return err
		}
		seen(x)
		if !ok {
			res.Skipped++
			continue
		}
		out.rows = append(out.rows, x)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s on %s: %w", kind.what, r.database, err)
	}
	return nil
}

// dropPending logs and counts pending ids given up.
func (r *Reconciler) dropPending(what string, ids []int64, why string,
	res *ReconcileResult) {
	if len(ids) == 0 {
		return
	}
	res.Dropped += len(ids)
	r.svc.cfg.logf("autonomy: stop waiting for the outcome of %d %s on %s (%s): "+
		"action_log ids %v; their outcomes are not recorded", len(ids), what, r.database,
		why, ids)
}

// nextIncidentCursor moves cur to the largest aged forward id (never back)
// and keeps pending the undone ids at or below it; ids above it are read
// forward again. Over incidentPendingCap the oldest ids are returned as
// dropped.
func nextIncidentCursor(cur incidentCursor, aged *int64, rows []executed,
	undone map[int64]bool) (incidentCursor, []int64) {
	next := incidentCursor{cursor: cur.cursor}
	if aged != nil && (next.cursor == nil || *aged > *next.cursor) {
		v := *aged
		next.cursor = &v
	}
	if next.cursor == nil {
		return next, nil
	}
	for _, x := range rows {
		if undone[x.actionLogID] && x.actionLogID <= *next.cursor {
			next.pending = append(next.pending, x.actionLogID)
		}
	}
	slices.Sort(next.pending)
	next.pending = slices.Compact(next.pending)
	if over := len(next.pending) - incidentPendingCap; over > 0 {
		dropped := slices.Clone(next.pending[:over])
		next.pending = slices.Clone(next.pending[over:])
		return next, dropped
	}
	return next, nil
}
