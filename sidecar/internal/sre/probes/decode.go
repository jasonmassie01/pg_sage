package probes

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Typed decoders for the rows the causal graph reads. Unavailable
// results are an *UnavailableError, never an empty list; numeric fields
// that are NULL or absent decode as NaN (unknown), never zero.

// LockEdge is one wait edge: WaiterPID waits for a lock BlockerPID holds
// (BlockerPID 0 is a prepared transaction).
type LockEdge struct {
	WaiterPID           int
	WaiterState         string
	LockType            string
	RequestedMode       string
	Relation            string
	BlockerPID          int
	BlockerKind         string
	BlockerState        string
	BlockerWaiting      bool
	BlockerXactAgeS     float64
	BlockerBackendStart time.Time
}

var strongRelationModes = map[string]bool{"AccessExclusiveLock": true,
	"ExclusiveLock": true, "ShareRowExclusiveLock": true, "ShareLock": true}

// StrongRelationWait reports a wait for a DDL-strength relation lock
// (the lock-queue amplifier: every later reader queues behind it).
func (e LockEdge) StrongRelationWait() bool {
	return e.LockType == "relation" && strongRelationModes[e.RequestedMode]
}

// RowWait reports a wait on a row (its transaction id or tuple lock).
func (e LockEdge) RowWait() bool {
	return e.LockType == "transactionid" || e.LockType == "tuple"
}

// LockRoot is one root blocker from lock_chains.
type LockRoot struct {
	PID          int
	Kind         string
	State        string
	XactAgeS     float64
	ChainDepth   int
	TotalBlocked int
}

// LongXact is one open transaction from long_transactions.
type LongXact struct {
	PID       int
	State     string
	XactAgeS  float64
	StateAgeS float64
	Waiting   bool
}

// IdleInTransaction reports an idle (or aborted) open transaction.
func (x LongXact) IdleInTransaction() bool {
	return strings.HasPrefix(x.State, "idle in transaction")
}

// PreparedXact is one prepared transaction in this database.
type PreparedXact struct {
	GIDHash string
	AgeS    float64
	XIDAge  int64
}

// PlanShift is one query's latency before and after its split point
// (its latest plan flip, or mid-window when the plan did not change).
type PlanShift struct {
	QueryID      int64
	PreviousHash string
	CurrentHash  string
	Flipped      bool
	FlippedAt    time.Time
	BeforeCalls  int64
	BeforeMeanMS float64
	AfterCalls   int64
	AfterMeanMS  float64
}

// Ratio is after/before mean latency; NaN when the baseline is unknown.
func (s PlanShift) Ratio() float64 {
	if !(s.BeforeMeanMS > 0) || math.IsNaN(s.AfterMeanMS) {
		return math.NaN()
	}
	return s.AfterMeanMS / s.BeforeMeanMS
}

func rowsFor(res Result, id ID) ([]Row, error) {
	if res.ProbeID != id {
		return nil, fmt.Errorf("result of %s is not %s", res.ProbeID, id)
	}
	if !res.Status.Usable() {
		return nil, &UnavailableError{ProbeID: id, Status: res.Status,
			Reason: res.Reason, Phase: res.Phase, Timing: res.Timing}
	}
	return res.Rows, nil
}

// LockEdges decodes a lock_graph result.
func LockEdges(res Result) ([]LockEdge, error) {
	rows, err := rowsFor(res, LockGraph)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]LockEdge, 0, len(rows))
	for i, r := range rows {
		waiter, err1 := intField(r, "waiter_pid")
		blocker, err2 := intField(r, "blocker_pid")
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("lock_graph row %d: %w", i+1, firstErr(err1, err2))
		}
		out = append(out, LockEdge{WaiterPID: int(waiter),
			WaiterState: strField(r, "waiter_state"), LockType: strField(r, "lock_type"),
			RequestedMode: strField(r, "requested_mode"), Relation: strField(r, "relation"),
			BlockerPID: int(blocker), BlockerKind: strField(r, "blocker_kind"),
			BlockerState: strField(r, "blocker_state"), BlockerWaiting: boolField(r,
				"blocker_waiting"), BlockerXactAgeS: floatField(r, "blocker_xact_age_s"),
			BlockerBackendStart: timeField(r, "blocker_backend_start")})
	}
	return out, nil
}

// LockRoots decodes a lock_chains result.
func LockRoots(res Result) ([]LockRoot, error) {
	rows, err := rowsFor(res, LockChains)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]LockRoot, 0, len(rows))
	for i, r := range rows {
		pid, err := intField(r, "root_pid")
		if err != nil {
			return nil, fmt.Errorf("lock_chains row %d: %w", i+1, err)
		}
		depth, _ := intField(r, "chain_depth")
		blocked, _ := intField(r, "total_blocked")
		out = append(out, LockRoot{PID: int(pid), Kind: strField(r, "root_kind"),
			State: strField(r, "root_state"), XactAgeS: floatField(r, "root_xact_age_s"),
			ChainDepth: int(depth), TotalBlocked: int(blocked)})
	}
	return out, nil
}

// LongXacts decodes a long_transactions result.
func LongXacts(res Result) ([]LongXact, error) {
	rows, err := rowsFor(res, LongTransactions)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]LongXact, 0, len(rows))
	for i, r := range rows {
		pid, err := intField(r, "pid")
		if err != nil {
			return nil, fmt.Errorf("long_transactions row %d: %w", i+1, err)
		}
		out = append(out, LongXact{PID: int(pid), State: strField(r, "state"),
			XactAgeS: floatField(r, "xact_age_s"), StateAgeS: floatField(r, "state_age_s"),
			Waiting: boolField(r, "waiting")})
	}
	return out, nil
}

// Prepared decodes a prepared_xacts result.
func Prepared(res Result) ([]PreparedXact, error) {
	rows, err := rowsFor(res, PreparedXacts)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]PreparedXact, 0, len(rows))
	for _, r := range rows {
		age, _ := intField(r, "xid_age")
		out = append(out, PreparedXact{GIDHash: strField(r, "gid_hash"),
			AgeS: floatField(r, "prepared_age_s"), XIDAge: age})
	}
	return out, nil
}

// PlanShifts decodes a plan_regressions result.
func PlanShifts(res Result) ([]PlanShift, error) {
	rows, err := rowsFor(res, PlanRegressions)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]PlanShift, 0, len(rows))
	for i, r := range rows {
		qid, err := intField(r, "queryid")
		if err != nil {
			return nil, fmt.Errorf("plan_regressions row %d: %w", i+1, err)
		}
		bc, _ := intField(r, "before_calls")
		ac, _ := intField(r, "after_calls")
		out = append(out, PlanShift{QueryID: qid,
			PreviousHash: strField(r, "previous_plan_hash"),
			CurrentHash:  strField(r, "current_plan_hash"),
			Flipped:      boolField(r, "plan_flipped"), FlippedAt: timeField(r, "flipped_at"),
			BeforeCalls: bc, BeforeMeanMS: floatField(r, "before_mean_ms"),
			AfterCalls: ac, AfterMeanMS: floatField(r, "after_mean_ms")})
	}
	return out, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func intField(r Row, key string) (int64, error) {
	switch v := r[key].(type) {
	case int64:
		return v, nil
	case int32:
		return int64(v), nil
	case int:
		return int64(v), nil
	case float64:
		if v == math.Trunc(v) {
			return int64(v), nil
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
	case nil:
		return 0, fmt.Errorf("%s is missing", key)
	}
	return 0, fmt.Errorf("%s is not an integer: %v", key, r[key])
}

func floatField(r Row, key string) float64 {
	switch v := r[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int32:
		return float64(v)
	case int:
		return float64(v)
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return math.NaN()
}

func strField(r Row, key string) string {
	s, _ := r[key].(string)
	return s
}

func boolField(r Row, key string) bool {
	b, _ := r[key].(bool)
	return b
}

func timeField(r Row, key string) time.Time {
	switch v := r[key].(type) {
	case time.Time:
		return v.UTC()
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
