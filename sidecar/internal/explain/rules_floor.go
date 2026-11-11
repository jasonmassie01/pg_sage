package explain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// Insight kinds and sources (tuning-10). Rule entries come from the
// deterministic plan scan the query tuner uses; model entries from the
// LLM, which may add entries but never removes a rule entry.
const (
	InsightSlowBecause    = "slow_because"
	InsightRecommendation = "recommendation"
	SourceRules           = "rules"
	SourceLLM             = "llm"
)

// Insight is one entry of slow_because or recommendations, tagged with
// where it came from.
type Insight struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Source string `json:"source"`
	// Symptom is the plan-scan symptom behind a rule entry.
	Symptom string `json:"symptom,omitempty"`
}

// maxSummaryReasons is how many rule reasons the rules summary names.
const maxSummaryReasons = 3

// applyRules is the deterministic floor: tuner.ScanPlan over the plan,
// with the catalog facts of the relations it names, fills slow_because,
// recommendations and the summary. A plan without actuals skips the rules
// that need an executed plan. A failed scan is logged and leaves the
// lists empty.
func (ex *Explainer) applyRules(ctx context.Context, result *ExplainResult, analyzed bool) {
	symptoms, err := scanRules(result.PlanJSON, analyzed, ex.catalogFacts(ctx, result.PlanJSON))
	if err != nil {
		ex.logFn("WARN", "explain: plan rules skipped: %v", err)
	}
	addRuleEntries(result, symptoms)
}

// catalogFacts reads the facts the seq-scan and parallel rules need, under
// the explain timeout. Without them those rules are skipped.
func (ex *Explainer) catalogFacts(ctx context.Context, plan json.RawMessage,
) *tuner.CatalogFacts {
	if ex.pool == nil || len(plan) == 0 {
		return nil
	}
	names, err := tuner.PlanRelationNames(plan)
	if err != nil || len(names) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, ex.timeout())
	defer cancel()
	facts, err := tuner.LoadCatalogFacts(ctx, ex.pool, names)
	if err != nil {
		ex.logFn("WARN", "explain: catalog facts unavailable, seq-scan and parallel "+
			"rules skipped: %v", err)
		return nil
	}
	return facts
}

// scanRules runs the tuner's plan scan with explain's thresholds (the
// shipped tuner defaults).
func scanRules(plan json.RawMessage, analyzed bool, facts *tuner.CatalogFacts,
) ([]tuner.PlanSymptom, error) {
	if len(plan) == 0 {
		return nil, nil
	}
	opts := []tuner.ScanOption{
		tuner.WithCatalogFacts(facts, config.DefaultTunerParallelMinRows),
	}
	if !analyzed {
		opts = append(opts, tuner.WithEstimatesOnly())
	}
	return tuner.ScanPlan(plan, opts...)
}

// addRuleEntries adds one slow_because and one recommendation per symptom
// (deduplicated) and writes the rules summary.
func addRuleEntries(result *ExplainResult, symptoms []tuner.PlanSymptom) {
	ensureLists(result)
	var reasons []string
	for _, s := range symptoms {
		because, recommend := tuner.Describe(s)
		if result.addEntry(InsightSlowBecause, because, SourceRules, string(s.Kind)) {
			reasons = append(reasons, strings.TrimSpace(because))
		}
		result.addEntry(InsightRecommendation, recommend, SourceRules, string(s.Kind))
	}
	result.Summary = ruleSummary(result.NodeBreakdown, reasons)
}

// ruleSummary is the root node and the first rule reasons, for example
// "Hash Join; sort spilled 8.2 MB to disk".
func ruleSummary(nodes []NodeExplain, reasons []string) string {
	if len(nodes) == 0 {
		return "the plan could not be read"
	}
	root := nodes[0].Description
	if len(reasons) == 0 {
		return root + "; the plan rules found no problem"
	}
	shown := reasons
	if len(shown) > maxSummaryReasons {
		shown = shown[:maxSummaryReasons]
	}
	s := root + "; " + strings.Join(shown, "; ")
	if more := len(reasons) - len(shown); more > 0 {
		s += fmt.Sprintf("; and %d more", more)
	}
	return s
}

func ensureLists(r *ExplainResult) {
	if r.SlowBecause == nil {
		r.SlowBecause = []string{}
	}
	if r.Recommendations == nil {
		r.Recommendations = []string{}
	}
	if r.Insights == nil {
		r.Insights = []Insight{}
	}
}

// addEntry appends text to the kind's list and to Insights unless it is
// empty or the list already says it; it reports whether it was added.
func (r *ExplainResult) addEntry(kind, text, source, symptom string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	list := &r.SlowBecause
	if kind == InsightRecommendation {
		list = &r.Recommendations
	}
	for _, have := range *list {
		if sameEntry(have, text) {
			return false
		}
	}
	*list = append(*list, text)
	r.Insights = append(r.Insights, Insight{Kind: kind, Text: text, Source: source,
		Symptom: symptom})
	return true
}

// sameEntry compares entries ignoring case, spacing and a final period.
func sameEntry(a, b string) bool {
	return entryKey(a) == entryKey(b)
}

func entryKey(s string) string {
	return strings.TrimRight(strings.ToLower(strings.Join(strings.Fields(s), " ")), ".")
}
