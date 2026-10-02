package runbook

import (
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Truth is a three-valued predicate result. Unknown means the evidence
// cannot decide (a probe failed, was not observed, or a truncated result
// cannot prove the condition): a runbook abstains instead of branching.
type Truth int

// Truth values.
const (
	Unknown Truth = iota
	False
	True
)

func (t Truth) String() string {
	switch t {
	case True:
		return "true"
	case False:
		return "false"
	}
	return "unknown"
}

func truthOf(b bool) Truth {
	if b {
		return True
	}
	return False
}

// Env is what a predicate reads: the latest result of each probe and the
// current diagnosis' hypothesis statuses.
type Env interface {
	Latest(id probes.ID) (probes.Result, bool)
	Hypothesis(node causal.NodeID) (causal.Status, bool)
}

// Eval evaluates p deterministically. The reason says why the result is
// unknown (or which leaf decided a false all / true any).
func Eval(p Predicate, env Env) (Truth, string) {
	switch p.Op {
	case OpAll, OpAny:
		return evalJunction(p, env)
	case OpNot:
		if len(p.Of) != 1 {
			return Unknown, "not needs one predicate"
		}
		t, why := Eval(p.Of[0], env)
		switch t {
		case True:
			return False, why
		case False:
			return True, why
		}
		return Unknown, why
	case OpHypothesis:
		st, ok := env.Hypothesis(causal.NodeID(p.Node))
		return truthOf(ok && containsString(p.In, string(st))),
			fmt.Sprintf("hypothesis %s is %s", p.Node, statusText(st, ok))
	}
	return evalProbe(p, env)
}

func statusText(st causal.Status, ok bool) string {
	if !ok {
		return "not in the diagnosis"
	}
	return string(st)
}

// evalJunction: all is false if any part is false, else unknown if any is
// unknown; any is true if any part is true, else unknown if any is unknown.
func evalJunction(p Predicate, env Env) (Truth, string) {
	decisive, absorbing := False, True
	if p.Op == OpAll {
		decisive, absorbing = True, False
	}
	unknown, why := false, ""
	for _, c := range p.Of {
		t, reason := Eval(c, env)
		switch {
		case t == absorbing:
			return absorbing, reason
		case t == Unknown && !unknown:
			unknown, why = true, reason
		}
	}
	if unknown {
		return Unknown, why
	}
	if len(p.Of) == 0 {
		return Unknown, p.Op + " has no predicates"
	}
	return decisive, ""
}

// evalProbe evaluates the leaves that read a probe result.
func evalProbe(p Predicate, env Env) (Truth, string) {
	r, ok := env.Latest(probes.ID(p.Probe))
	if !ok {
		return Unknown, fmt.Sprintf("probe %s was not observed", p.Probe)
	}
	if p.Op == OpProbeStatus {
		return truthOf(containsString(p.In, string(r.Status))),
			fmt.Sprintf("probe %s is %s", p.Probe, r.Status)
	}
	if !r.Status.Usable() {
		return Unknown, fmt.Sprintf("probe %s %s (%s)", p.Probe, r.Status, r.Reason)
	}
	switch p.Op {
	case OpRowCount:
		return evalRowCount(p, r)
	case OpColumn:
		return evalColumn(p, r)
	case OpColumnText:
		return evalText(p, r)
	}
	return Unknown, fmt.Sprintf("unknown operator %q", p.Op)
}

func truncated(p Predicate) string {
	return fmt.Sprintf("probe %s returned a truncated result; %s %s %s cannot be proven",
		p.Probe, p.Op, p.Cmp, formatValue(p.Value))
}

func formatValue(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(*v)
}

func evalRowCount(p Predicate, r probes.Result) (Truth, string) {
	if p.Value == nil {
		return Unknown, "row_count needs a value"
	}
	n := float64(len(r.Rows))
	t := truthOf(compare(n, p.Cmp, *p.Value))
	why := fmt.Sprintf("probe %s returned %d rows", p.Probe, len(r.Rows))
	if r.Truncated && !(t == True && (p.Cmp == ">" || p.Cmp == ">=")) {
		return Unknown, truncated(p)
	}
	return t, why
}

// columnValues reads a column's numbers (nulls skipped). ok is false when
// no row has the column or a value is not a number.
func columnValues(p Predicate, r probes.Result) ([]float64, string, bool) {
	var vals []float64
	present := false
	for _, row := range r.Rows {
		v, has := row[p.Column]
		if !has {
			continue
		}
		present = true
		if v == nil {
			continue
		}
		f, isNum := number(v)
		if !isNum {
			return nil, fmt.Sprintf("column %s of probe %s is not a number", p.Column,
				p.Probe), false
		}
		vals = append(vals, f)
	}
	if len(r.Rows) > 0 && !present {
		return nil, fmt.Sprintf("probe %s returned no column %s", p.Probe, p.Column), false
	}
	return vals, "", true
}

func evalColumn(p Predicate, r probes.Result) (Truth, string) {
	vals, why, ok := columnValues(p, r)
	if !ok || p.Value == nil {
		return Unknown, why
	}
	t := aggregate(p.Agg, vals, p.Cmp, *p.Value)
	if r.Truncated && !provenOnPrefix(p, t) {
		return Unknown, truncated(p)
	}
	return t, fmt.Sprintf("%s(%s of %s) %s %s is %s", p.Agg, p.Column, p.Probe, p.Cmp,
		formatValue(p.Value), t)
}

// provenOnPrefix reports whether a result over the rows seen holds for
// the full (truncated) result as well.
func provenOnPrefix(p Predicate, t Truth) bool {
	if t != True {
		return false
	}
	switch p.Agg {
	case AggMax:
		return p.Cmp == ">" || p.Cmp == ">="
	case AggMin:
		return p.Cmp == "<" || p.Cmp == "<="
	case AggAny:
		return true
	}
	return false
}

// aggregate compares a column aggregate. Over no numbers max, min, any
// and all are false; sum is 0.
func aggregate(agg string, vals []float64, cmp string, v float64) Truth {
	switch agg {
	case AggSum:
		total := 0.0
		for _, x := range vals {
			total += x
		}
		return truthOf(compare(total, cmp, v))
	case AggAny, AggAll:
		hits := 0
		for _, x := range vals {
			if compare(x, cmp, v) {
				hits++
			}
		}
		if agg == AggAny {
			return truthOf(hits > 0)
		}
		return truthOf(len(vals) > 0 && hits == len(vals))
	}
	if len(vals) == 0 {
		return False
	}
	best := vals[0]
	for _, x := range vals[1:] {
		if (agg == AggMax && x > best) || (agg == AggMin && x < best) {
			best = x
		}
	}
	return truthOf(compare(best, cmp, v))
}

func evalText(p Predicate, r probes.Result) (Truth, string) {
	present := false
	for _, row := range r.Rows {
		v, has := row[p.Column]
		present = present || has
		if s, isText := v.(string); isText && s == p.Text {
			return True, fmt.Sprintf("a row of %s has %s = %q", p.Probe, p.Column, p.Text)
		}
	}
	if len(r.Rows) > 0 && !present {
		return Unknown, fmt.Sprintf("probe %s returned no column %s", p.Probe, p.Column)
	}
	if r.Truncated {
		return Unknown, truncated(p)
	}
	return False, fmt.Sprintf("no row of %s has %s = %q", p.Probe, p.Column, p.Text)
}

func compare(a float64, cmp string, b float64) bool {
	switch cmp {
	case ">":
		return a > b
	case ">=":
		return a >= b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case "==":
		return a == b
	case "!=":
		return a != b
	}
	return false
}

// number reads the numeric types a probe row holds: int64 and float64 from
// a live run, json.Number from stored evidence.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}
