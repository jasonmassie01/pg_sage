package runbook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Decision predicates are deterministic and three-valued: a failed or
// missing observation is unknown, never "healthy" (AI-SRE-SPEC §2.3).
// Unknown makes the runbook abstain rather than take a branch.

type env struct {
	results map[probes.ID]probes.Result
	nodes   map[causal.NodeID]causal.Status
}

func (e env) Latest(id probes.ID) (probes.Result, bool) {
	r, ok := e.results[id]
	return r, ok
}

func (e env) Hypothesis(id causal.NodeID) (causal.Status, bool) {
	s, ok := e.nodes[id]
	return s, ok
}

func result(id probes.ID, st probes.Status, rows ...probes.Row) probes.Result {
	return probes.Result{ProbeID: id, Version: "v1", Status: st, Rows: rows}
}

func ok(id probes.ID, rows ...probes.Row) probes.Result {
	if len(rows) == 0 {
		return result(id, probes.StatusEmpty)
	}
	return result(id, probes.StatusOK, rows...)
}

func envOf(rs ...probes.Result) env {
	e := env{results: map[probes.ID]probes.Result{}, nodes: map[causal.NodeID]causal.Status{
		causal.IdleInTxHolder: causal.StatusRoot, causal.DDLLockQueue: causal.StatusContributing,
		causal.HotRowContention: causal.StatusRuledOut}}
	for _, r := range rs {
		e.results[r.ProbeID] = r
	}
	return e
}

// ages has three transactions: 30 s (int64), 450.5 s (float64) and
// 120 s (json.Number, as stored evidence decodes), plus one null age.
func ages() probes.Result {
	return ok(probes.LongTransactions,
		probes.Row{"pid": int64(1), "state": "active", "xact_age_s": int64(30)},
		probes.Row{"pid": int64(2), "state": "idle in transaction", "xact_age_s": 450.5},
		probes.Row{"pid": int64(3), "state": "active", "xact_age_s": json.Number("120")},
		probes.Row{"pid": int64(4), "state": "active", "xact_age_s": nil})
}

func col(agg, cmp string, v float64) Predicate {
	return Predicate{Op: OpColumn, Probe: "long_transactions", Column: "xact_age_s",
		Agg: agg, Cmp: cmp, Value: num(v)}
}

func expect(t *testing.T, name string, p Predicate, e env, want Truth) string {
	t.Helper()
	got, why := Eval(p, e)
	if got != want {
		t.Errorf("%s: Eval = %s (%s), want %s", name, got, why, want)
	}
	return why
}

func TestEval_ColumnAggregatesOverMixedNumericTypes(t *testing.T) {
	e := envOf(ages())
	expect(t, "max >= 450.5", col(AggMax, ">=", 450.5), e, True)
	expect(t, "max > 450.5", col(AggMax, ">", 450.5), e, False)
	expect(t, "min == 30", col(AggMin, "==", 30), e, True)
	expect(t, "sum == 600.5", col(AggSum, "==", 600.5), e, True)
	expect(t, "any > 400", col(AggAny, ">", 400), e, True)
	expect(t, "any > 500", col(AggAny, ">", 500), e, False)
	expect(t, "all > 10", col(AggAll, ">", 10), e, True)
	expect(t, "all > 100", col(AggAll, ">", 100), e, False)
	expect(t, "max != 450.5", col(AggMax, "!=", 450.5), e, False)
	expect(t, "min < 31", col(AggMin, "<", 31), e, True)
	expect(t, "min <= 29", col(AggMin, "<=", 29), e, False)
}

func TestEval_ColumnUnknownWhenItCannotBeRead(t *testing.T) {
	missing := expect(t, "column absent", Predicate{Op: OpColumn,
		Probe: "long_transactions", Column: "backend_xmin_age", Agg: AggMax, Cmp: ">",
		Value: num(1)}, envOf(ages()), Unknown)
	if !strings.Contains(missing, "backend_xmin_age") {
		t.Errorf("reason %q does not name the column", missing)
	}
	text := ok(probes.LongTransactions, probes.Row{"xact_age_s": "old"})
	expect(t, "non-numeric", col(AggMax, ">", 1), envOf(text), Unknown)
	failed := result(probes.LongTransactions, probes.StatusError)
	failed.Reason = "statement_timeout"
	why := expect(t, "probe error", col(AggMax, ">", 1), envOf(failed), Unknown)
	if !strings.Contains(why, "statement_timeout") && !strings.Contains(why, "error") {
		t.Errorf("reason %q does not say the probe failed", why)
	}
	for _, st := range []probes.Status{probes.StatusNoPrivilege, probes.StatusUnsupported} {
		expect(t, string(st), col(AggAny, ">", 1),
			envOf(result(probes.LongTransactions, st)), Unknown)
	}
	why = expect(t, "never observed", col(AggMax, ">", 1), envOf(), Unknown)
	if !strings.Contains(why, "long_transactions") {
		t.Errorf("reason %q does not name the probe", why)
	}
}

func TestEval_AggregatesOverNoRows(t *testing.T) {
	e := envOf(ok(probes.LongTransactions))
	expect(t, "max over none", col(AggMax, ">", 0), e, False)
	expect(t, "min over none", col(AggMin, "<", 1e9), e, False)
	expect(t, "any over none", col(AggAny, ">", 0), e, False)
	expect(t, "all over none", col(AggAll, ">", 0), e, False)
	expect(t, "sum over none is 0", col(AggSum, "==", 0), e, True)
	onlyNull := ok(probes.LongTransactions, probes.Row{"xact_age_s": nil})
	expect(t, "nulls only", col(AggMax, ">", 0), envOf(onlyNull), False)
}

func TestEval_TruncatedResultsOnlyProveLowerBounds(t *testing.T) {
	r := ages()
	r.Truncated = true
	e := envOf(r)
	expect(t, "max >= seen", col(AggMax, ">=", 450), e, True)
	expect(t, "any > seen", col(AggAny, ">", 400), e, True)
	expect(t, "min <= seen", col(AggMin, "<=", 30), e, True)
	expect(t, "max < x cannot be proven", col(AggMax, "<", 1000), e, Unknown)
	expect(t, "all cannot be proven", col(AggAll, ">", 10), e, Unknown)
	expect(t, "sum cannot be proven", col(AggSum, "==", 600.5), e, Unknown)
	expect(t, "rows >= seen", Predicate{Op: OpRowCount, Probe: "long_transactions",
		Cmp: ">=", Value: num(4)}, e, True)
	expect(t, "rows < x cannot be proven", Predicate{Op: OpRowCount,
		Probe: "long_transactions", Cmp: "<", Value: num(10)}, e, Unknown)
	expect(t, "rows == seen cannot be proven", Predicate{Op: OpRowCount,
		Probe: "long_transactions", Cmp: "==", Value: num(4)}, e, Unknown)
}

func TestEval_RowCountAndStatus(t *testing.T) {
	rc := func(cmp string, v float64) Predicate {
		return Predicate{Op: OpRowCount, Probe: "long_transactions", Cmp: cmp, Value: num(v)}
	}
	expect(t, "4 rows", rc("==", 4), envOf(ages()), True)
	expect(t, "empty is 0", rc("==", 0), envOf(ok(probes.LongTransactions)), True)
	expect(t, "error is unknown", rc("==", 0),
		envOf(result(probes.LongTransactions, probes.StatusError)), Unknown)
	st := func(in ...string) Predicate {
		return Predicate{Op: OpProbeStatus, Probe: "long_transactions", In: in}
	}
	expect(t, "ok in ok", st("ok", "empty"), envOf(ages()), True)
	expect(t, "error not in ok", st("ok"),
		envOf(result(probes.LongTransactions, probes.StatusError)), False)
	expect(t, "error is a known status", st("error", "no_privilege"),
		envOf(result(probes.LongTransactions, probes.StatusError)), True)
	expect(t, "not observed", st("ok"), envOf(), Unknown)
}

func TestEval_ColumnText(t *testing.T) {
	p := Predicate{Op: OpColumnText, Probe: "long_transactions", Column: "state",
		Text: "idle in transaction"}
	expect(t, "one row matches", p, envOf(ages()), True)
	p.Text = "idle in transaction (aborted)"
	expect(t, "no row matches", p, envOf(ages()), False)
	p.Text = "IDLE IN TRANSACTION"
	expect(t, "case sensitive", p, envOf(ages()), False)
	p.Column = "wait_event"
	expect(t, "missing column", p, envOf(ages()), Unknown)
	p.Column = "state"
	expect(t, "probe failed", p, envOf(result(probes.LongTransactions,
		probes.StatusError)), Unknown)
}

func TestEval_HypothesisStatus(t *testing.T) {
	h := func(node string, in ...string) Predicate {
		return Predicate{Op: OpHypothesis, Node: node, In: in}
	}
	e := envOf()
	expect(t, "root", h("idle_in_tx_holder", "root_cause"), e, True)
	expect(t, "contributing", h("ddl_lock_queue", "root_cause", "contributing"), e, True)
	expect(t, "ruled out", h("hot_row_contention", "root_cause"), e, False)
	expect(t, "ruled out matches", h("hot_row_contention", "ruled_out"), e, True)
	expect(t, "absent from diagnosis", h("prepared_xact_holder", "root_cause",
		"contributing", "unproven"), e, False)
}

func TestEval_ThreeValuedCombinators(t *testing.T) {
	T := Predicate{Op: OpHypothesis, Node: "idle_in_tx_holder", In: []string{"root_cause"}}
	F := Predicate{Op: OpHypothesis, Node: "idle_in_tx_holder", In: []string{"ruled_out"}}
	U := Predicate{Op: OpProbeStatus, Probe: "lock_graph", In: []string{"ok"}}
	e := envOf()
	all := func(ps ...Predicate) Predicate { return Predicate{Op: OpAll, Of: ps} }
	any := func(ps ...Predicate) Predicate { return Predicate{Op: OpAny, Of: ps} }
	not := func(p Predicate) Predicate { return Predicate{Op: OpNot, Of: []Predicate{p}} }
	cases := []struct {
		name string
		p    Predicate
		want Truth
	}{
		{"all T T", all(T, T), True}, {"all T F", all(T, F), False},
		{"all T U", all(T, U), Unknown}, {"all F U", all(F, U), False},
		{"any F F", any(F, F), False}, {"any F T", any(F, T), True},
		{"any F U", any(F, U), Unknown}, {"any T U", any(T, U), True},
		{"not T", not(T), False}, {"not F", not(F), True}, {"not U", not(U), Unknown},
		{"nested", all(T, any(F, not(F))), True},
	}
	for _, c := range cases {
		expect(t, c.name, c.p, e, c.want)
	}
}

func TestTruth_String(t *testing.T) {
	for truth, want := range map[Truth]string{True: "true", False: "false",
		Unknown: "unknown"} {
		if truth.String() != want {
			t.Errorf("%d.String() = %q, want %q", truth, truth.String(), want)
		}
	}
}
