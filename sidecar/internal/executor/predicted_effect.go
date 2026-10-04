package executor

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Phase 1.3: every action carries a structured predicted effect, recorded
// before it runs (before_state.predicted_effect and a pending
// sage.action_outcome row). A producer may supply one in its finding
// detail ("predicted_effect"); otherwise it is derived here per class.
// Without one the action has "no prediction" and is never credited.

// predictionFromDetail derives an action's predicted effect from its
// finding detail.
func predictionFromDetail(class string, detail map[string]any) verify.Prediction {
	if p, ok := producerPrediction(class, detail); ok {
		return p
	}
	switch class {
	case verify.ClassIndexCreate, verify.ClassQueryHint:
		return estimatedLatencyPrediction(class, detail)
	case verify.ClassIndexDrop:
		p := ruleBasedPrediction(class, verify.MetricMeanExecTime, 0,
			"reads of the table are expected unchanged; the index's space and write cost "+
				"are freed")
		p.TargetQueryIDs = targetQueryIDs(analyzer.Finding{Detail: detail})
		return p
	case verify.ClassVacuum:
		return ruleBasedPrediction(class, verify.MetricDeadTuples, -100,
			"VACUUM removes the table's dead tuples")
	case verify.ClassAnalyze:
		return ruleBasedPrediction(class, verify.MetricModsSinceAnalyze, -100,
			"ANALYZE resets the rows modified since the last analyze")
	case verify.ClassGUC, verify.ClassReloption:
		return verify.NoPrediction(class, "no targeted metric to verify this change")
	case verify.ClassStatistics, verify.ClassReindex:
		return r2Prediction(class, detail)
	}
	return verify.NoPrediction(class, "no predicted effect for this kind of action")
}

// producerPrediction is a prediction the producer put in the detail; a
// malformed one is ignored (the derived prediction applies).
func producerPrediction(class string, detail map[string]any) (verify.Prediction, bool) {
	raw, ok := detail["predicted_effect"].(map[string]any)
	if !ok {
		return verify.Prediction{}, false
	}
	raw = exactTargetIDs(raw)
	encoded, err := json.Marshal(raw)
	if err != nil {
		return verify.Prediction{}, false
	}
	var p verify.Prediction
	if json.Unmarshal(encoded, &p) != nil || !p.Predicts() {
		return verify.Prediction{}, false
	}
	p.Class = class
	return p, true
}

// exactTargetIDs reads target queryids a producer wrote as decimal strings
// (the tuning agent does: they survive a JSON round trip that decodes
// numbers as float64) as exact integers. A malformed entry is left as is,
// so the prediction is ignored.
func exactTargetIDs(raw map[string]any) map[string]any {
	ids, ok := raw["target_queryids"].([]any)
	if !ok {
		if strs, isStrs := raw["target_queryids"].([]string); isStrs {
			ids = make([]any, len(strs))
			for i, s := range strs {
				ids[i] = s
			}
		} else {
			return raw
		}
	}
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
		if s, isStr := id.(string); isStr {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				out[i] = n
			}
		}
	}
	copied := make(map[string]any, len(raw))
	for k, v := range raw {
		copied[k] = v
	}
	copied["target_queryids"] = out
	return copied
}

// estimatedLatencyPrediction turns a producer's estimated improvement into
// a predicted fall of the targets' mean execution time: HypoPG-backed
// when the what-if gate verified it, the model's estimate otherwise.
func estimatedLatencyPrediction(class string, detail map[string]any) verify.Prediction {
	pct, ok := detailFloat(detail["estimated_improvement_pct"])
	if !ok || !(pct > 0) {
		return verify.NoPrediction(class, "the producer gave no estimated improvement")
	}
	method, source := verify.MethodModel, "tuner"
	if class == verify.ClassIndexCreate {
		source = "optimizer"
	}
	if verdict, _ := detail["what_if_verdict"].(string); verdict == "verified" {
		method = verify.MethodHypoPG
	}
	expected := -pct
	confidence, _ := detailFloat(detail["confidence_score"])
	return verify.Prediction{Class: class, Method: method, Metric: verify.MetricMeanExecTime,
		TargetQueryIDs:    targetQueryIDs(analyzer.Finding{Detail: detail}),
		ExpectedChangePct: &expected, Confidence: confidence, Source: source}
}

func ruleBasedPrediction(class, metric string, change float64, note string) verify.Prediction {
	return verify.Prediction{Class: class, Method: verify.MethodRule, Metric: metric,
		ExpectedChangePct: &change, Source: "rule", Note: note}
}

func detailFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// predictAction records in before (the action's before_state) its
// predicted effect and what the verifier needs to judge it: the targeted
// queries with their frozen pre-action baseline, and for maintenance the
// metric's value now. It runs after the config prior state is recorded
// and before the action's SQL.
func (e *Executor) predictAction(
	ctx context.Context, sql string, detail map[string]any, before map[string]any,
) verify.Prediction {
	p := e.predictEffect(ctx, sql, detail, before)
	if len(p.TargetQueryIDs) > 0 {
		before["target_queryids"] = p.TargetQueryIDs
		before["verify_baseline"] = e.freezeBaseline(ctx, verificationClass(sql),
			p.TargetQueryIDs)
	}
	before["predicted_effect"] = p
	return p
}

// predictEffect is an action's predicted effect with its targeted queries
// and, for maintenance, the metric's value now; it reads statistics and
// catalogs only. Shadow decisions (roadmap 1.4) record the same
// prediction without freezing a verification baseline.
func (e *Executor) predictEffect(
	ctx context.Context, sql string, detail map[string]any, before map[string]any,
) verify.Prediction {
	class := verificationClass(sql)
	p := predictionFromDetail(class, detail)
	switch class {
	case verify.ClassGUC, verify.ClassReloption:
		if metric := recordedConfigMetric(before); metric != "" && !p.Predicts() {
			p = configPrediction(metric)
			p.Class = class
		}
		p.TargetQueryIDs = e.configTargets(ctx, sql, before)
	case verify.ClassIndexDrop:
		p.TargetQueryIDs = e.dropTargets(ctx, sql, before)
	case verify.ClassVacuum, verify.ClassAnalyze:
		if class == verify.ClassVacuum && strings.Contains(strings.ToUpper(sql), "FREEZE") {
			p = ruleBasedPrediction(class, verify.MetricFrozenXIDAge, -100,
				"VACUUM (FREEZE) advances the table's relfrozenxid")
		}
		e.maintenanceBaseline(ctx, sql, &p, before)
	case verify.ClassStatistics:
		e.statisticsBaseline(ctx, sql, &p, before)
	case verify.ClassReindex:
		e.reindexBaseline(ctx, sql, &p, before)
	}
	return p
}

// recordedConfigMetric is the targeted metric of the config change
// recorded in before (configChange.record), if any.
func recordedConfigMetric(before map[string]any) string {
	change, _ := before["config_change"].(map[string]any)
	if b, ok := change["outcome"].(*outcomeBaseline); ok && b != nil {
		return b.Metric
	}
	return ""
}

// predictionIn is the prediction predictAction stored in before.
func predictionIn(before map[string]any) (verify.Prediction, bool) {
	p, ok := before["predicted_effect"].(verify.Prediction)
	return p, ok
}
