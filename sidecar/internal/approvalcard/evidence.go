package approvalcard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Evidence bounds: a card cites the producer's numbers, not its dump.
const (
	maxMetricEvidence = 12
	maxQueries        = 5
	maxEvidenceValue  = 120
)

// nonEvidenceKeys are detail keys that are not cited facts: SQL, model
// text, the predicted effect and the gate inputs (shown elsewhere on the
// card), and internal identities.
var nonEvidenceKeys = map[string]bool{
	"ddl": true, "drop_ddl": true, "llm_rationale": true, "rationale": true,
	"narrative": true, "rewrite_rationale": true, "index_fingerprint": true,
	"category": true, "index_category": true, "action_level": true, "action_risk": true,
	"what_if_verdict": true, "what_if_reason": true, "hypopg_validated": true,
	"affected_queries": true, "queryids": true, "approval_required": true,
	"partition_plan": true, "partitioned_parent": true, "estimated_improvement_pct": true,
	"estimated_size_bytes": true, "confidence_score": true, "table": true,
	"suggested_rewrite": true, "query": true, "recommended_sql": true,
	"proposed_sql": true, "plan_source": true,
}

// evidenceOf cites the finding, its numbers, the queries and plan it is
// about, the recommendation revision and the policy decision.
func evidenceOf(in Inputs) []Evidence {
	out := []Evidence{}
	seen := map[string]bool{}
	if f := in.Finding; f != nil {
		ref := "finding:" + itoa(f.ID)
		out = append(out, Evidence{Kind: "finding", Label: "Finding #" + itoa(f.ID),
			Value: findingSummary(f), Ref: ref})
		out = append(out, metricEvidence(f.Detail, ref, seen)...)
		out = append(out, queryEvidence(f.Detail, ref)...)
		if src, ok := f.Detail["plan_source"].(string); ok && src != "" {
			out = append(out, Evidence{Kind: "plan", Label: "plan source", Value: src,
				Ref: ref})
		}
	}
	if r := in.Revision; r != nil {
		ref := fmt.Sprintf("recommendation:%d@%d", r.ID, r.Revision)
		out = append(out, Evidence{Kind: "recommendation",
			Label: fmt.Sprintf("Recommendation #%d revision %d", r.ID, r.Revision),
			Value: r.ContentHash, Ref: ref})
		out = append(out, metricEvidence(r.Evidence, ref, seen)...)
	}
	if d := in.Decision; d != nil {
		out = append(out, Evidence{Kind: "decision",
			Label: fmt.Sprintf("Policy decision #%d", d.ID),
			Value: fmt.Sprintf("%s (%s risk): %s", d.Verdict, d.RiskTier, humanize(d.Reason)),
			Ref:   fmt.Sprintf("decision:%d", d.ID)})
	}
	return capMetrics(out)
}

func findingSummary(f *FindingRow) string {
	parts := []string{}
	for _, s := range []string{f.Title, f.Severity, f.Category} {
		if strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " | ")
}

// metricEvidence cites every scalar detail value, sorted by key.
func metricEvidence(detail map[string]any, ref string, seen map[string]bool) []Evidence {
	keys := make([]string, 0, len(detail))
	for k := range detail {
		if !nonEvidenceKeys[k] && !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []Evidence
	for _, k := range keys {
		value, ok := scalar(detail[k])
		if !ok {
			continue
		}
		seen[k] = true
		out = append(out, Evidence{Kind: "metric", Label: strings.ReplaceAll(k, "_", " "),
			Value: value, Ref: ref})
	}
	return out
}

// capMetrics keeps at most maxMetricEvidence metric items.
func capMetrics(items []Evidence) []Evidence {
	out := items[:0]
	metrics := 0
	for _, e := range items {
		if e.Kind == "metric" {
			if metrics == maxMetricEvidence {
				continue
			}
			metrics++
		}
		out = append(out, e)
	}
	return out
}

func queryEvidence(detail map[string]any, ref string) []Evidence {
	texts := queryTexts(detail["affected_queries"])
	ids := queryIDs(detail["queryids"])
	var out []Evidence
	for i := 0; i < len(texts) || (i < len(ids) && i < maxQueries); i++ {
		e := Evidence{Kind: "query", Label: "Query", Ref: ref}
		if i < len(ids) {
			e.Label = "Query " + strconv.FormatInt(ids[i], 10)
			e.Ref = "queryid:" + strconv.FormatInt(ids[i], 10)
		}
		if i < len(texts) {
			e.Value = truncate(texts[i], 200)
		}
		out = append(out, e)
	}
	return out
}

// scalar renders a number, boolean or short string; anything else is not
// a citable value.
func scalar(v any) (string, bool) {
	if n, ok := number(v); ok {
		return strconv.FormatFloat(n, 'f', -1, 64), true
	}
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t), true
	case string:
		if t != "" && len(t) <= maxEvidenceValue {
			return t, true
		}
	}
	return "", false
}

// number reads a JSON number of any decoded Go type.
func number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func humanize(code string) string { return strings.ReplaceAll(code, "_", " ") }

// truncate shortens s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
