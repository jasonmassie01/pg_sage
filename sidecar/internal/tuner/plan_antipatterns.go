package tuner

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Plan anti-patterns (market-10) that the index-missing rules do not
// catch. Both read an executed plan and are advisory: Prescribe gives them
// no hint, and pg_sage never rewrites a statement.

// defaultAntipatternMinRemoved is the rows a filter must discard over all
// loops before an anti-pattern is worth reporting.
const defaultAntipatternMinRemoved = 10_000

// indexFilterMinShareTenths: the index filter must discard at least 9/10
// of the rows the index read (compared in integers-as-floats, so 0.90 is
// exact).
const indexFilterMinShareTenths = 9

// WithIndexFilterMinRemoved sets the rows an index scan's filter must
// discard (Rows Removed by Filter x Actual Loops) before
// index_filter_inefficient fires; n <= 0 uses the 10,000 default.
func WithIndexFilterMinRemoved(n int64) ScanOption {
	return func(sc *scanContext) { sc.indexFilterMinRemoved = n }
}

// WithORJoinMinRemoved sets the rows a nested loop's OR join filter must
// discard (Rows Removed by Join Filter x Actual Loops) before
// or_join_filter fires; n <= 0 uses the 10,000 default.
func WithORJoinMinRemoved(n int64) ScanOption {
	return func(sc *scanContext) { sc.orJoinMinRemoved = n }
}

func (sc *scanContext) indexFilterThreshold() float64 {
	if sc == nil {
		return minRemoved(0)
	}
	return minRemoved(sc.indexFilterMinRemoved)
}

func (sc *scanContext) orJoinThreshold() float64 {
	if sc == nil {
		return minRemoved(0)
	}
	return minRemoved(sc.orJoinMinRemoved)
}

func minRemoved(n int64) float64 {
	if n <= 0 {
		return defaultAntipatternMinRemoved
	}
	return float64(n)
}

// nodeLoops is Actual Loops, or 1 when it is missing or not positive.
func nodeLoops(n planNode) int64 {
	if n.ActualLoops != nil && *n.ActualLoops > 0 {
		return *n.ActualLoops
	}
	return 1
}

// checkIndexFilterInefficient flags an index or bitmap heap scan whose
// filter discards at least 90% of the rows the index read, and at least
// the threshold in total: the index serves only part of the predicate.
func checkIndexFilterInefficient(n planNode, depth int, sc *scanContext) *PlanSymptom {
	switch n.NodeType {
	case "Index Scan", "Index Only Scan", "Bitmap Heap Scan":
	default:
		return nil
	}
	if n.ActualRows == nil || n.RowsRemovedByFilter == nil || n.Filter == nil {
		return nil
	}
	loops := nodeLoops(n)
	removed := *n.RowsRemovedByFilter * float64(loops)
	kept := *n.ActualRows * float64(loops)
	if removed < sc.indexFilterThreshold() ||
		removed*10 < (removed+kept)*indexFilterMinShareTenths {
		return nil
	}
	index, cond := scanIndex(n)
	cols := sortedKeys(filterColumns(*n.Filter))
	return &PlanSymptom{
		Kind: SymptomIndexFilterInefficient, NodeType: n.NodeType, NodeDepth: depth,
		RelationName: n.RelationName, Schema: n.Schema, Alias: n.Alias, IndexName: index,
		Detail: map[string]any{
			"index_name": index, "index_cond": cond, "filter": *n.Filter,
			"filter_columns": cols, "rows_removed": int64(math.Round(removed)),
			"rows_kept": int64(math.Round(kept)), "removed_share": removed / (removed + kept),
			"loops": loops,
		},
	}
}

// scanIndex is the index a scan reads and its Index Cond; a Bitmap Heap
// Scan takes them from its single Bitmap Index Scan input ("" for a
// BitmapAnd / BitmapOr).
func scanIndex(n planNode) (index, cond string) {
	src := n
	if n.NodeType == "Bitmap Heap Scan" {
		if len(n.Plans) != 1 || n.Plans[0].NodeType != "Bitmap Index Scan" {
			return "", ""
		}
		src = n.Plans[0]
	}
	if src.IndexCond != nil {
		cond = *src.IndexCond
	}
	return src.IndexName, cond
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkORJoinFilter flags a nested loop whose Join Filter is a top-level
// OR whose arms reference both inputs (some arm the outer, some arm the
// inner), discarding at least the threshold over all loops: each arm could
// use its own index if the statement were a UNION.
func checkORJoinFilter(n planNode, depth int, sc *scanContext) *PlanSymptom {
	if n.NodeType != "Nested Loop" || n.JoinFilter == nil ||
		n.RowsRemovedByJoinFilter == nil || n.ActualRows == nil {
		return nil
	}
	loops := nodeLoops(n)
	removed := *n.RowsRemovedByJoinFilter * float64(loops)
	if removed < sc.orJoinThreshold() {
		return nil
	}
	arms := topLevelSplit(unwrapParens(strings.TrimSpace(*n.JoinFilter)), " OR ")
	outer, inner := nestedLoopChildren(n)
	if len(arms) < 2 || outer == nil || inner == nil {
		return nil
	}
	aliases, ok := orArmsSpanInputs(arms, subtreeAliases(*outer), subtreeAliases(*inner))
	if !ok {
		return nil
	}
	trimmed := make([]string, len(arms))
	for i, a := range arms {
		trimmed[i] = strings.TrimSpace(a)
	}
	return &PlanSymptom{
		Kind: SymptomORJoinFilter, NodeType: n.NodeType, NodeDepth: depth,
		JoinAliases: joinAliases(n),
		Detail: map[string]any{
			"join_filter": *n.JoinFilter, "arms": trimmed, "aliases": aliases,
			"rows_removed": int64(math.Round(removed)), "loops": loops,
		},
	}
}

// orArmsSpanInputs reports whether every arm references an alias of the
// outer or inner input and the arms together reference both. aliases are
// the referenced ones, outer first.
func orArmsSpanInputs(arms []string, outer, inner []string) (aliases []string, ok bool) {
	var usedOuter, usedInner bool
	used := map[string]bool{}
	for _, arm := range arms {
		refs := referencedAliases(arm)
		armOuter, armInner := anyIn(refs, outer), anyIn(refs, inner)
		if !armOuter && !armInner {
			return nil, false
		}
		usedOuter, usedInner = usedOuter || armOuter, usedInner || armInner
		for r := range refs {
			used[r] = true
		}
	}
	if !usedOuter || !usedInner {
		return nil, false
	}
	for _, a := range append(append([]string{}, outer...), inner...) {
		if used[a] {
			aliases = append(aliases, a)
			used[a] = false
		}
	}
	return aliases, true
}

func anyIn(refs map[string]bool, aliases []string) bool {
	for _, a := range aliases {
		if refs[a] {
			return true
		}
	}
	return false
}

// subtreeAliases is the alias of n and of the relations below it (SubPlan
// and InitPlan subtrees excluded, as in joinAliases).
func subtreeAliases(n planNode) []string {
	out := joinAliases(n)
	if n.Alias != "" && !containsString(out, n.Alias) {
		out = append([]string{n.Alias}, out...)
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var (
	quotedLiteral   = regexp.MustCompile(`'(?:[^']|'')*'`)
	aliasQualifier  = regexp.MustCompile(`(?:^|[^a-z0-9_$".])([a-z_][a-z0-9_$]*)\.`)
	quotedQualifier = regexp.MustCompile(`"((?:[^"]|"")+)"\.`)
)

// referencedAliases returns the qualifiers (alias.column) in an EXPLAIN
// expression, ignoring text inside string literals.
func referencedAliases(expr string) map[string]bool {
	expr = quotedLiteral.ReplaceAllString(expr, "''")
	out := map[string]bool{}
	for _, m := range aliasQualifier.FindAllStringSubmatch(expr, -1) {
		out[m[1]] = true
	}
	for _, m := range quotedQualifier.FindAllStringSubmatch(expr, -1) {
		out[strings.ReplaceAll(m[1], `""`, `"`)] = true
	}
	return out
}

// prescribeIndexFilter is advisory: it names the index, how much its
// filter discards, and the composite index that would read only the kept
// rows (index cond columns, then the filter's equality columns).
func prescribeIndexFilter(s PlanSymptom) *Prescription {
	removed, _ := s.Detail["rows_removed"].(int64)
	kept, _ := s.Detail["rows_kept"].(int64)
	share, _ := s.Detail["removed_share"].(float64)
	cond, _ := s.Detail["index_cond"].(string)
	filter, _ := s.Detail["filter"].(string)
	subject := "the bitmap scan of " + s.RelationName
	if s.IndexName != "" {
		subject = "index " + s.IndexName
	}
	text := fmt.Sprintf("%s reads %d rows per execution and its filter discards %.0f%%",
		subject, removed+kept, math.Floor(share*100+1e-9))
	if cols := suggestedIndexColumns(cond, filter); len(cols) > 0 {
		return advisory(s.Kind, text+fmt.Sprintf(
			"; an index on (%s) would read only the kept rows", strings.Join(cols, ", ")))
	}
	return advisory(s.Kind, text+"; an index that also covers the filter would read "+
		"only the kept rows")
}

// suggestedIndexColumns is the index cond's columns then the filter's
// equality columns (all its columns when none is an equality), or nil
// when the filter has no usable column.
func suggestedIndexColumns(cond, filter string) []string {
	filterCols, equality := conjunctColumns(filter)
	if len(filterCols) == 0 {
		return nil
	}
	cols, _ := conjunctColumns(cond)
	cols = append([]string{}, cols...)
	for _, c := range filterCols {
		if (len(equality) == 0 || equality[c]) && !containsString(cols, c) {
			cols = append(cols, c)
		}
	}
	return cols
}

// prescribeORJoin is advisory: the UNION rewrite is the operator's to make.
func prescribeORJoin(s PlanSymptom) *Prescription {
	aliases, _ := s.Detail["aliases"].([]string)
	removed, _ := s.Detail["rows_removed"].(int64)
	return advisory(s.Kind, fmt.Sprintf("the nested loop's join filter discards %d rows: "+
		"the OR across %s is applied after the join; rewriting as UNION (or UNION ALL "+
		"with an exclusion predicate) lets each arm use its own index. "+
		"pg_sage does not rewrite statements", removed, strings.Join(aliases, " and ")))
}

func advisory(kind SymptomKind, rationale string) *Prescription {
	return &Prescription{Symptom: kind, Rationale: rationale}
}

// withAntipatterns adds plan_antipatterns to a query_tuning finding: one
// entry per anti-pattern symptom with its plan detail, so the advice names
// the index, filter and rows it is based on.
func withAntipatterns(f analyzer.Finding, symptoms []PlanSymptom) analyzer.Finding {
	var out []map[string]any
	for _, s := range symptoms {
		if s.Kind != SymptomIndexFilterInefficient && s.Kind != SymptomORJoinFilter {
			continue
		}
		entry := map[string]any{"symptom": string(s.Kind)}
		for k, v := range s.Detail {
			entry[k] = v
		}
		if s.RelationName != "" {
			entry["relation"] = CanonicalTableName(s.Schema, s.RelationName)
		}
		out = append(out, entry)
	}
	if len(out) > 0 && f.Detail != nil {
		f.Detail["plan_antipatterns"] = out
	}
	return f
}
