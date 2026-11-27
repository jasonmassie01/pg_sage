package tuner

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strconv"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Parameterization advice (tuning-11): a statement shape the application
// sends with a different IN-list length each time is one pg_stat_statements
// entry per length before PG18, each planned on its own; a statement whose
// planning dominates its execution re-plans every call. Both get a
// query_parameterization finding (advice only, no SQL), which the cases
// projector turns into a prepare_parameterized_query script.

// QuerySource is the collector's latest snapshot. The detector reads its
// queries, so it adds no read of pg_stat_statements.
type QuerySource interface {
	LatestSnapshot() *collector.Snapshot
}

const (
	// DefaultParamShapeMinVariants is tuner.param_shape_min_variants unset.
	DefaultParamShapeMinVariants = 20
	// paramMaxQueryIDs caps the queryids one finding lists.
	paramMaxQueryIDs = 20
	// pgSquashesLists is the first major whose pg_stat_statements squashes
	// IN and ARRAY lists into one entry.
	pgSquashesLists     = 18
	paramRecommendation = "Use prepared statements or a single array parameter " +
		"(= ANY($1)) so the statement has one plan-cache entry; on PG18 lists are squashed."
)

// paramList is one bind parameter, optionally cast ($1, $2::int8).
const paramList = `\$\d+(?:\s*::\s*[A-Za-z_][\w.]*(?:\[\])?)?`

var (
	inListRe = regexp.MustCompile(`(?i)\b(IN)\s*\(\s*` + paramList +
		`(?:\s*,\s*` + paramList + `)*\s*\)`)
	anyListRe = regexp.MustCompile(`(?i)=\s*(ANY)\s*\(\s*ARRAY\s*\[\s*` + paramList +
		`(?:\s*,\s*` + paramList + `)*\s*\]\s*\)`)
)

// WithQuerySource feeds the detector the collector's snapshots and the
// server's version (server_version_num; 0 when unknown). Set before use.
func (t *Tuner) WithQuerySource(src QuerySource, pgVersionNum int) {
	t.querySource = src
	t.pgMajor = pgVersionNum / 10000
}

// collapseLists replaces every IN ($1, ...) and = ANY (ARRAY[$1, ...]) list
// of bind parameters with (...); list is false when the text had none. A
// subquery or a list of literals (an unnormalized utility statement) is not
// a list of parameters and stays.
func collapseLists(query string) (collapsed string, list bool) {
	out := inListRe.ReplaceAllString(query, "${1} (...)")
	out = anyListRe.ReplaceAllString(out, "= ${1} (...)")
	return out, out != query
}

func shapeIdentifier(collapsed string) string {
	sum := sha256.Sum256([]byte(collapsed))
	return "shape:" + hex.EncodeToString(sum[:8])
}

func (t *Tuner) minVariants() int {
	if t.cfg.ParamShapeMinVariants > 0 {
		return t.cfg.ParamShapeMinVariants
	}
	return DefaultParamShapeMinVariants
}

// paramShape accumulates the statements of one collapsed shape.
type paramShape struct {
	text           string
	ids            map[int64]bool
	calls          int64
	execMS, planMS float64
	reason         string
}

// parameterizationFindings starts the cycle's set of reported shapes and
// returns the IN-list findings of the latest snapshot. The caller holds
// t.mu (Tune).
func (t *Tuner) parameterizationFindings() []analyzer.Finding {
	t.paramEmitted = map[string]bool{}
	if t.querySource == nil || t.pgMajor >= pgSquashesLists {
		return nil
	}
	snap := t.querySource.LatestSnapshot()
	if snap == nil {
		return nil
	}
	shapes := groupListShapes(snap.Queries)
	var out []analyzer.Finding
	for _, s := range shapes {
		if len(s.ids) < t.minVariants() {
			continue
		}
		s.reason = "in_list_variants"
		f := t.paramFinding(s)
		t.paramEmitted[f.ObjectIdentifier] = true
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b analyzer.Finding) int {
		return cmp.Compare(a.ObjectIdentifier, b.ObjectIdentifier)
	})
	return out
}

// groupListShapes groups the workload statements that hold a parameter
// list by their collapsed text.
func groupListShapes(queries []collector.QueryStats) map[string]*paramShape {
	shapes := map[string]*paramShape{}
	for _, q := range queries {
		if q.Query == "" || q.QueryID == 0 || workload.Excluded(q.Query) {
			continue
		}
		collapsed, list := collapseLists(q.Query)
		if !list {
			continue
		}
		s := shapes[collapsed]
		if s == nil {
			s = &paramShape{text: collapsed, ids: map[int64]bool{}}
			shapes[collapsed] = s
		}
		if s.ids[q.QueryID] {
			continue
		}
		s.ids[q.QueryID] = true
		s.calls += q.Calls
		s.execMS += q.TotalExecTime
		s.planMS += q.TotalPlanTime
	}
	return shapes
}

// extractPlanTime takes the high-plan-time symptom out of a candidate's
// symptoms: it prescribes no hint, and the statement gets parameterization
// advice instead, unless its shape was reported this cycle.
func (t *Tuner) extractPlanTime(
	c candidate, symptoms []PlanSymptom,
) ([]PlanSymptom, []analyzer.Finding) {
	kept := make([]PlanSymptom, 0, len(symptoms))
	planTime := false
	for _, s := range symptoms {
		if s.Kind == SymptomHighPlanTime {
			planTime = true
			continue
		}
		kept = append(kept, s)
	}
	if !planTime {
		return symptoms, nil
	}
	collapsed, _ := collapseLists(c.Query)
	s := &paramShape{text: collapsed, ids: map[int64]bool{c.QueryID: true}, calls: c.Calls,
		execMS: c.MeanExecTime * float64(c.Calls), planMS: c.MeanPlanTime * float64(c.Calls),
		reason: "plan_time_share"}
	f := t.paramFinding(s)
	if t.paramEmitted == nil {
		t.paramEmitted = map[string]bool{}
	}
	if t.paramEmitted[f.ObjectIdentifier] {
		return kept, nil
	}
	t.paramEmitted[f.ObjectIdentifier] = true
	return kept, []analyzer.Finding{f}
}

func (t *Tuner) paramFinding(s *paramShape) analyzer.Finding {
	ids := make([]int64, 0, len(s.ids))
	for id := range s.ids {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	share := 0.0
	if s.planMS > 0 {
		share = s.planMS / (s.planMS + s.execMS)
	}
	detail := map[string]any{
		"normalized_query": s.text, "reason": s.reason, "variant_count": len(ids),
		"queryids": ids[:min(len(ids), paramMaxQueryIDs)], "calls": s.calls,
		"total_exec_ms": s.execMS, "plan_time_share": share, "pg_major": t.pgMajor,
	}
	return analyzer.Finding{
		Category:         "query_parameterization",
		Severity:         "warning",
		ObjectType:       "query_shape",
		ObjectIdentifier: shapeIdentifier(s.text),
		Title:            paramTitle(s.reason, len(ids)),
		Detail:           detail,
		Recommendation:   paramRecommendation,
		ActionRisk:       "moderate",
	}
}

func paramTitle(reason string, variants int) string {
	if reason == "in_list_variants" {
		return "Query shape sent with " + strconv.Itoa(variants) +
			" different IN-list lengths: parameterize the list"
	}
	return "Planning dominates this statement: use a prepared statement"
}
