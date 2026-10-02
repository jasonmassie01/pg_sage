package runbook

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Predicate is a decision node's deterministic condition. Leaves read the
// latest result of one catalog probe or one hypothesis of the causal
// graph's diagnosis; all/any/not combine them.
type Predicate struct {
	Op     string      `json:"op"`
	Of     []Predicate `json:"of,omitempty"`
	Probe  string      `json:"probe,omitempty"`
	In     []string    `json:"in,omitempty"`
	Column string      `json:"column,omitempty"`
	Agg    string      `json:"agg,omitempty"`
	Cmp    string      `json:"cmp,omitempty"`
	Value  *float64    `json:"value,omitempty"`
	Text   string      `json:"text,omitempty"`
	Node   string      `json:"node,omitempty"`
}

// Predicate operators.
const (
	OpAll         = "all"
	OpAny         = "any"
	OpNot         = "not"
	OpProbeStatus = "probe_status"
	OpRowCount    = "row_count"
	OpColumn      = "column"
	OpColumnText  = "column_text"
	OpHypothesis  = "hypothesis"
)

// Column aggregates: max, min and sum of the column's numbers; any (some
// row satisfies the comparison) and all (every row with a number does).
const (
	AggMax = "max"
	AggMin = "min"
	AggSum = "sum"
	AggAny = "any"
	AggAll = "all"
)

var (
	comparisons = map[string]bool{">": true, ">=": true, "<": true, "<=": true,
		"==": true, "!=": true}
	aggregates = map[string]bool{AggMax: true, AggMin: true, AggSum: true, AggAny: true,
		AggAll: true}
	probeStatuses = map[string]bool{string(probes.StatusOK): true,
		string(probes.StatusEmpty): true, string(probes.StatusError): true,
		string(probes.StatusNoPrivilege): true, string(probes.StatusUnsupported): true}
	hypothesisStatuses = map[string]bool{string(causal.StatusRoot): true,
		string(causal.StatusContributing): true, string(causal.StatusAlternative): true,
		string(causal.StatusRuledOut): true}
)

// setFields names the fields a predicate sets, in a fixed order, for the
// per-op shape check.
func (p Predicate) setFields() []string {
	all := []struct {
		name string
		set  bool
	}{{"of", len(p.Of) > 0}, {"probe", p.Probe != ""}, {"in", len(p.In) > 0},
		{"column", p.Column != ""}, {"agg", p.Agg != ""}, {"cmp", p.Cmp != ""},
		{"value", p.Value != nil}, {"text", p.Text != ""}, {"node", p.Node != ""}}
	var out []string
	for _, f := range all {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// opFields are the fields each operator uses (required unless noted in
// checkLeaf).
var opFields = map[string][]string{
	OpAll: {"of"}, OpAny: {"of"}, OpNot: {"of"},
	OpProbeStatus: {"probe", "in"}, OpRowCount: {"probe", "cmp", "value"},
	OpColumn:     {"probe", "column", "agg", "cmp", "value"},
	OpColumnText: {"probe", "column", "text"}, OpHypothesis: {"node", "in"},
}

// validatePredicate appends the problems of p at path.
func validatePredicate(p Predicate, path string, depth int, out *Problems) {
	if depth > MaxPredicateDepth {
		out.add(CodePredicate, path, "predicates nest at most %d deep", MaxPredicateDepth)
		return
	}
	fields, ok := opFields[p.Op]
	if !ok {
		out.add(CodePredicate, path+".op", "unknown operator %q", p.Op)
		return
	}
	allowed := map[string]bool{}
	for _, f := range fields {
		allowed[f] = true
	}
	for _, f := range p.setFields() {
		if !allowed[f] {
			out.add(CodePredicate, path+"."+f, "%s does not take %s", p.Op, f)
		}
	}
	switch p.Op {
	case OpAll, OpAny, OpNot:
		checkChildren(p, path, depth, out)
	case OpHypothesis:
		checkHypothesis(p, path, out)
	default:
		checkProbeLeaf(p, path, out)
	}
}

func checkChildren(p Predicate, path string, depth int, out *Problems) {
	switch {
	case p.Op == OpNot && len(p.Of) != 1:
		out.add(CodePredicate, path+".of", "not takes exactly one predicate")
	case len(p.Of) == 0:
		out.add(CodePredicate, path+".of", "%s needs at least one predicate", p.Op)
	case len(p.Of) > MaxPredicateChildren:
		out.add(CodePredicate, path+".of", "%s takes at most %d predicates", p.Op,
			MaxPredicateChildren)
	}
	for i, c := range p.Of {
		validatePredicate(c, fmt.Sprintf("%s.of[%d]", path, i), depth+1, out)
	}
}

func checkHypothesis(p Predicate, path string, out *Problems) {
	if _, ok := causal.NodeByID(causal.NodeID(p.Node)); !ok {
		out.add(CodeUnknownNode, path+".node", "%q is not a causal graph node", p.Node)
	}
	checkStatuses(p.In, hypothesisStatuses, path, out)
}

func checkStatuses(in []string, known map[string]bool, path string, out *Problems) {
	if len(in) == 0 {
		out.add(CodePredicate, path+".in", "name at least one status")
	}
	for _, s := range in {
		if !known[s] {
			out.add(CodePredicate, path+".in", "unknown status %q", s)
		}
	}
}

func checkProbeLeaf(p Predicate, path string, out *Problems) {
	if _, ok := probes.Catalog().Spec(probes.ID(p.Probe)); !ok {
		out.add(CodeUnknownProbe, path+".probe", "%q is not a catalog probe", p.Probe)
		return
	}
	switch p.Op {
	case OpProbeStatus:
		checkStatuses(p.In, probeStatuses, path, out)
	case OpRowCount:
		checkComparison(p, path, out)
		if p.Value != nil && *p.Value < 0 {
			out.add(CodePredicate, path+".value", "a row count is never negative")
		}
	case OpColumn:
		checkColumn(p, path, out)
		if !aggregates[p.Agg] {
			out.add(CodePredicate, path+".agg", "unknown aggregate %q", p.Agg)
		}
		checkComparison(p, path, out)
	case OpColumnText:
		checkColumn(p, path, out)
		if p.Text == "" || utf8.RuneCountInString(p.Text) > MaxTextRunes ||
			strings.IndexFunc(p.Text, isControl) >= 0 {
			out.add(CodePredicate, path+".text", "text must be 1-%d printable characters",
				MaxTextRunes)
		}
	}
}

func checkColumn(p Predicate, path string, out *Problems) {
	if !HasColumn(probes.ID(p.Probe), p.Column) {
		out.add(CodeUnknownColumn, path+".column", "probe %s returns no column %q",
			p.Probe, p.Column)
	}
}

func checkComparison(p Predicate, path string, out *Problems) {
	if !comparisons[p.Cmp] {
		out.add(CodePredicate, path+".cmp", "unknown comparison %q", p.Cmp)
	}
	if p.Value == nil || math.IsNaN(*p.Value) || math.IsInf(*p.Value, 0) {
		out.add(CodePredicate, path+".value", "a finite number is required")
	}
}
