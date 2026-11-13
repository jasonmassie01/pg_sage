package tuner

import (
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// market-10b: a hash join needs an equality between the two join inputs.
// A nested loop joined only by OR (or by no condition at all) has none, so
// PostgreSQL cannot build a hash join and pg_hint_plan cannot force one.

// joinSide is the join input an operand of an equality belongs to.
type joinSide int

const (
	sideNone joinSide = iota
	sideOuter
	sideInner
)

// columnRef is an optionally alias-qualified column as EXPLAIN prints it:
// b.a_id, a_id, "B"."Weird Col".
var columnRef = regexp.MustCompile(`^(?:([a-z_][a-z0-9_$]*|"(?:[^"]|"")+")\.)?` +
	`([a-z_][a-z0-9_$]*|"(?:[^"]|"")+")$`)

// hashableJoin reports whether a nested loop has a clause a hash join could
// use: an equality between a column of the outer input and a column of the
// inner input, as a top-level AND conjunct of the Join Filter or of the
// Index Cond, Recheck Cond or Filter of a parameterized inner scan (reached
// through Memoize, Materialize, Append and a Bitmap Heap Scan's Bitmap
// Index Scan). Anything else, an unparseable condition included, is false:
// fail closed, no join hint.
func hashableJoin(n planNode) bool {
	outer, inner := nestedLoopChildren(n)
	if outer == nil || inner == nil {
		return false
	}
	outerAliases, innerAliases := subtreeAliases(*outer), subtreeAliases(*inner)
	if n.JoinFilter != nil &&
		hasCrossEquality(*n.JoinFilter, outerAliases, innerAliases, false) {
		return true
	}
	for _, scan := range innerScans(*inner) {
		for _, cond := range []*string{scan.IndexCond, scan.RecheckCond, scan.Filter} {
			if cond != nil && hasCrossEquality(*cond, outerAliases, innerAliases, true) {
				return true
			}
		}
	}
	return false
}

// innerScans returns the nodes whose conditions may carry the nested
// loop's parameter: the inner node and, through pass-through nodes, the
// scans below it.
func innerScans(n planNode) []planNode {
	out := []planNode{n}
	switch n.NodeType {
	case "Memoize", "Materialize", "Append", "Merge Append":
		for _, child := range n.Plans {
			if child.ParentRelationship != "SubPlan" && child.ParentRelationship != "InitPlan" {
				out = append(out, innerScans(child)...)
			}
		}
	case "Bitmap Heap Scan":
		for _, child := range n.Plans {
			if child.NodeType == "Bitmap Index Scan" {
				out = append(out, child)
			}
		}
	}
	return out
}

// hasCrossEquality reports whether a top-level AND conjunct of expr is
// operand = operand with one operand on each input. scanQual marks a scan's
// own condition, where an unqualified column is the scanned (inner) one.
func hasCrossEquality(expr string, outer, inner []string, scanQual bool) bool {
	expr = unwrapParens(strings.TrimSpace(expr))
	if expr == "" || len(topLevelSplit(expr, " OR ")) > 1 {
		return false
	}
	for _, conjunct := range topLevelSplit(expr, " AND ") {
		sides := topLevelSplit(unwrapParens(strings.TrimSpace(conjunct)), " = ")
		if len(sides) != 2 {
			continue
		}
		l := operandSide(sides[0], outer, inner, scanQual)
		r := operandSide(sides[1], outer, inner, scanQual)
		if (l == sideOuter && r == sideInner) || (l == sideInner && r == sideOuter) {
			return true
		}
	}
	return false
}

// operandSide places a plain column operand (casts and parentheses
// stripped) on the outer or inner input; anything else is sideNone.
func operandSide(operand string, outer, inner []string, scanQual bool) joinSide {
	expr := unwrapParens(stripCasts(unwrapParens(strings.TrimSpace(operand))))
	m := columnRef.FindStringSubmatch(expr)
	if m == nil {
		return sideNone
	}
	if m[1] == "" {
		if scanQual {
			return sideInner
		}
		return sideNone
	}
	alias := strings.ReplaceAll(strings.Trim(m[1], `"`), `""`, `"`)
	inOuter, inInner := containsString(outer, alias), containsString(inner, alias)
	switch {
	case inOuter && !inInner:
		return sideOuter
	case inInner && !inOuter:
		return sideInner
	}
	return sideNone
}

// joinMayHash is false only when the scanner recorded hashable_join false.
// A symptom built without the key (not by checkBadNestedLoop, which always
// records it) keeps the earlier behaviour.
func joinMayHash(s PlanSymptom) bool {
	h, ok := s.Detail["hashable_join"].(bool)
	return !ok || h
}

// withJoinHintNote says on a query_tuning finding why a misestimated
// nested loop got no HashJoin hint: its join cannot be hashed.
func withJoinHintNote(f analyzer.Finding, symptoms []PlanSymptom) analyzer.Finding {
	var notes []string
	for _, s := range symptoms {
		if s.Kind != SymptomBadNestedLoop || joinMayHash(s) {
			continue
		}
		notes = append(notes, "no join hint: the nested loop joining "+
			strings.Join(s.JoinAliases, " and ")+" has no equality between its "+
			"inputs (only an OR, or no join condition), so PostgreSQL cannot "+
			"hash-join it and a join hint could not take effect")
	}
	if len(notes) == 0 || f.Detail == nil {
		return f
	}
	note := strings.Join(notes, "; ")
	f.Detail["join_hint_note"] = note
	if f.Recommendation == "" {
		f.Recommendation = note
	} else {
		f.Recommendation += "; " + note
	}
	return f
}
