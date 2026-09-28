package probes

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// Typed decoders turn a probe's rows into the structs the causal graph
// reads. Unavailable results are errors (never an empty, healthy list),
// and unknown values stay unknown (NaN), never zero.

func okResult(id ID, cols []string, rows ...Row) Result {
	st := StatusOK
	if len(rows) == 0 {
		st = StatusEmpty
	}
	return Result{ProbeID: id, Version: "v1", Status: st, Columns: cols,
		Rows: rows, ObservedAt: time.Now()}
}

func TestLockEdges_DecodesRowsOfEveryNumericShape(t *testing.T) {
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	res := okResult(LockGraph, nil,
		Row{"waiter_pid": int64(20), "waiter_state": "active",
			"lock_type": "relation", "requested_mode": "AccessExclusiveLock",
			"relation": "public.orders", "blocker_pid": int32(10),
			"blocker_kind": "backend", "blocker_state": "idle in transaction",
			"blocker_waiting": false, "blocker_xact_age_s": 75.5,
			"blocker_backend_start": start},
		Row{"waiter_pid": json.Number("30"), "lock_type": "relation",
			"requested_mode": "AccessShareLock", "blocker_pid": float64(20),
			"blocker_kind": "backend", "blocker_state": "active",
			"blocker_waiting": true, "blocker_xact_age_s": nil,
			"blocker_backend_start": start.Format(time.RFC3339Nano)},
	)
	edges, err := LockEdges(res)
	if err != nil {
		t.Fatalf("LockEdges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(edges))
	}
	e := edges[0]
	if e.WaiterPID != 20 || e.BlockerPID != 10 || e.RequestedMode !=
		"AccessExclusiveLock" || e.Relation != "public.orders" ||
		e.BlockerState != "idle in transaction" || e.BlockerXactAgeS != 75.5 ||
		!e.BlockerBackendStart.Equal(start) || e.BlockerWaiting {
		t.Fatalf("edge 0 = %+v", e)
	}
	if edges[1].WaiterPID != 30 || edges[1].BlockerPID != 20 ||
		!edges[1].BlockerWaiting || !math.IsNaN(edges[1].BlockerXactAgeS) {
		t.Fatalf("edge 1 = %+v (unknown age must be NaN)", edges[1])
	}
	if !edges[1].BlockerBackendStart.Equal(start) {
		t.Fatalf("RFC3339 backend_start not decoded: %v", edges[1].BlockerBackendStart)
	}
}

func TestLockEdges_StrongRelationMode(t *testing.T) {
	for mode, strong := range map[string]bool{
		"AccessExclusiveLock": true, "ExclusiveLock": true,
		"ShareRowExclusiveLock": true, "ShareLock": true,
		"ShareUpdateExclusiveLock": false, "RowExclusiveLock": false,
		"AccessShareLock": false, "": false,
	} {
		e := LockEdge{LockType: "relation", RequestedMode: mode}
		if e.StrongRelationWait() != strong {
			t.Errorf("%s strong = %v, want %v", mode, !strong, strong)
		}
	}
	row := LockEdge{LockType: "transactionid", RequestedMode: "ShareLock"}
	if row.StrongRelationWait() || !row.RowWait() {
		t.Error("a transactionid ShareLock wait is a row wait, not DDL")
	}
	if !(LockEdge{LockType: "tuple"}).RowWait() {
		t.Error("tuple waits are row waits")
	}
}

func TestDecoders_RejectUnavailableAndMismatchedResults(t *testing.T) {
	denied := Result{ProbeID: LockGraph, Status: StatusNoPrivilege,
		Reason: "insufficient_privilege"}
	_, err := LockEdges(denied)
	var ue *UnavailableError
	if !errors.As(err, &ue) || ue.Status != StatusNoPrivilege ||
		!strings.Contains(err.Error(), "insufficient_privilege") {
		t.Fatalf("err = %v, want UnavailableError(no_privilege)", err)
	}
	if _, err := LockEdges(okResult(LongTransactions, nil,
		Row{"pid": int64(1)})); err == nil {
		t.Fatal("LockEdges accepted a long_transactions result")
	}
	if _, err := PlanShifts(okResult(LockGraph, nil)); err == nil {
		t.Fatal("PlanShifts accepted a lock_graph result")
	}
	edges, err := LockEdges(okResult(LockGraph, nil))
	if err != nil || edges != nil {
		t.Fatalf("empty lock_graph = (%v, %v), want (nil, nil)", edges, err)
	}
}

func TestLockEdges_RejectsMissingIdentity(t *testing.T) {
	res := okResult(LockGraph, nil, Row{"waiter_pid": "abc",
		"blocker_pid": int64(1)})
	if _, err := LockEdges(res); err == nil {
		t.Fatal("a non-numeric waiter_pid must be rejected")
	}
	res = okResult(LockGraph, nil, Row{"blocker_pid": int64(1)})
	if _, err := LockEdges(res); err == nil {
		t.Fatal("a missing waiter_pid must be rejected")
	}
}

func TestLockRootsLongXactsPrepared_Decode(t *testing.T) {
	roots, err := LockRoots(okResult(LockChains, nil, Row{"root_pid": int64(7),
		"root_kind": "backend", "root_state": "active", "root_xact_age_s": 3.0,
		"chain_depth": int64(2), "total_blocked": int64(4)}))
	if err != nil || len(roots) != 1 || roots[0].PID != 7 ||
		roots[0].TotalBlocked != 4 || roots[0].ChainDepth != 2 ||
		roots[0].XactAgeS != 3 {
		t.Fatalf("roots = %+v err=%v", roots, err)
	}
	xs, err := LongXacts(okResult(LongTransactions, nil, Row{"pid": int64(9),
		"state": "idle in transaction", "xact_age_s": 120.0,
		"state_age_s": 119.0, "waiting": false}))
	if err != nil || len(xs) != 1 || xs[0].PID != 9 || xs[0].XactAgeS != 120 ||
		!xs[0].IdleInTransaction() {
		t.Fatalf("long xacts = %+v err=%v", xs, err)
	}
	ps, err := Prepared(okResult(PreparedXacts, nil,
		Row{"gid_hash": "ab", "prepared_age_s": 600.0, "xid_age": int64(12)}))
	if err != nil || len(ps) != 1 || ps[0].AgeS != 600 || ps[0].XIDAge != 12 {
		t.Fatalf("prepared = %+v err=%v", ps, err)
	}
}

func TestPlanShifts_DecodeFlipAndRatio(t *testing.T) {
	flipped := time.Now().UTC().Truncate(time.Second)
	res := okResult(PlanRegressions, nil,
		Row{"queryid": int64(42), "previous_plan_hash": "v1:a",
			"current_plan_hash": "v1:b", "plan_flipped": true,
			"flipped_at": flipped, "before_calls": int64(20),
			"before_mean_ms": 0.5, "after_calls": int64(20), "after_mean_ms": 5.0},
		Row{"queryid": int64(43), "previous_plan_hash": nil,
			"current_plan_hash": "v1:c", "plan_flipped": false,
			"flipped_at": nil, "before_calls": int64(5), "before_mean_ms": 0.0,
			"after_calls": int64(5), "after_mean_ms": 1.0})
	shifts, err := PlanShifts(res)
	if err != nil || len(shifts) != 2 {
		t.Fatalf("shifts = %+v err=%v", shifts, err)
	}
	s := shifts[0]
	if s.QueryID != 42 || !s.Flipped || s.PreviousHash != "v1:a" ||
		s.CurrentHash != "v1:b" || !s.FlippedAt.Equal(flipped) ||
		s.Ratio() != 10 {
		t.Fatalf("shift 0 = %+v ratio=%v", s, s.Ratio())
	}
	if !math.IsNaN(shifts[1].Ratio()) {
		t.Fatalf("zero baseline latency must give an unknown (NaN) ratio, "+
			"got %v", shifts[1].Ratio())
	}
	if shifts[1].Flipped || !shifts[1].FlippedAt.IsZero() {
		t.Fatalf("shift 1 = %+v, want not flipped", shifts[1])
	}
}
