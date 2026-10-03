package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/verify"
)

// No concurrent access tests: predictionFromDetail is a pure function of
// the finding detail.

func expected(t *testing.T, p verify.Prediction) float64 {
	t.Helper()
	if p.ExpectedChangePct == nil {
		t.Fatalf("prediction %+v has no expected change", p)
	}
	return *p.ExpectedChangePct
}

func TestPredictionFromOptimizerHypoPG(t *testing.T) {
	p := predictionFromDetail(verify.ClassIndexCreate, map[string]any{
		"estimated_improvement_pct": 35.0, "what_if_verdict": "verified",
		"hypopg_validated": true, "queryids": []any{float64(11), float64(12)},
		"confidence_score": 0.8,
	})
	if p.Method != verify.MethodHypoPG || p.Metric != verify.MetricMeanExecTime ||
		expected(t, p) != -35 || p.Confidence != 0.8 || p.Class != verify.ClassIndexCreate {
		t.Fatalf("prediction = %+v, want hypopg -35%% mean_exec_time", p)
	}
	if len(p.TargetQueryIDs) != 2 || p.TargetQueryIDs[0] != 11 || p.TargetQueryIDs[1] != 12 {
		t.Fatalf("targets = %v, want [11 12]", p.TargetQueryIDs)
	}
}

func TestPredictionFromModelEstimate(t *testing.T) {
	p := predictionFromDetail(verify.ClassIndexCreate, map[string]any{
		"estimated_improvement_pct": 25.0, "what_if_verdict": "unverified",
		"queryid": int64(7),
	})
	if p.Method != verify.MethodModel || expected(t, p) != -25 ||
		len(p.TargetQueryIDs) != 1 || p.TargetQueryIDs[0] != 7 {
		t.Fatalf("prediction = %+v, want model -25%% on [7]", p)
	}
}

func TestPredictionWithoutEstimateIsNoPrediction(t *testing.T) {
	for name, detail := range map[string]map[string]any{
		"nil detail":    nil,
		"no estimate":   {"queryids": []int64{1}},
		"zero estimate": {"estimated_improvement_pct": 0.0, "queryids": []int64{1}},
		"negative":      {"estimated_improvement_pct": -10.0, "queryids": []int64{1}},
		"wrong type":    {"estimated_improvement_pct": "lots", "queryids": []int64{1}},
	} {
		p := predictionFromDetail(verify.ClassIndexCreate, detail)
		if p.Predicts() || p.Method != verify.MethodNone || p.Note == "" {
			t.Errorf("%s: prediction = %+v, want an explained no-prediction", name, p)
		}
	}
}

func TestPredictionProducerSuppliedWins(t *testing.T) {
	p := predictionFromDetail(verify.ClassQueryHint, map[string]any{
		"predicted_effect": map[string]any{"method": "model", "metric": "mean_exec_time",
			"expected_change_pct": -60.0, "target_queryids": []any{float64(5)},
			"source": "tuner"},
		"estimated_improvement_pct": 10.0,
	})
	if p.Method != verify.MethodModel || expected(t, p) != -60 || p.Source != "tuner" ||
		p.Class != verify.ClassQueryHint || len(p.TargetQueryIDs) != 1 {
		t.Fatalf("prediction = %+v, want the producer's -60%% for query 5", p)
	}
}

func TestPredictionProducerSuppliedMalformedFallsBack(t *testing.T) {
	p := predictionFromDetail(verify.ClassIndexCreate, map[string]any{
		"predicted_effect":          "faster",
		"estimated_improvement_pct": 30.0, "what_if_verdict": "verified",
	})
	if p.Method != verify.MethodHypoPG || expected(t, p) != -30 {
		t.Fatalf("prediction = %+v, want the derived hypopg prediction", p)
	}
}

func TestPredictionForIndexDrop(t *testing.T) {
	p := predictionFromDetail(verify.ClassIndexDrop, map[string]any{"index_def": "x"})
	if p.Method != verify.MethodRule || expected(t, p) != 0 ||
		p.Metric != verify.MetricMeanExecTime {
		t.Fatalf("drop prediction = %+v, want rule: reads unchanged", p)
	}
}

func TestPredictionForMaintenance(t *testing.T) {
	vac := predictionFromDetail(verify.ClassVacuum, nil)
	if vac.Method != verify.MethodRule || vac.Metric != verify.MetricDeadTuples ||
		expected(t, vac) != -100 {
		t.Fatalf("vacuum prediction = %+v", vac)
	}
	an := predictionFromDetail(verify.ClassAnalyze, nil)
	if an.Method != verify.MethodRule || an.Metric != verify.MetricModsSinceAnalyze ||
		expected(t, an) != -100 {
		t.Fatalf("analyze prediction = %+v", an)
	}
}

func TestPredictionForUnknownClassIsNone(t *testing.T) {
	for _, class := range []string{"", "reindex", verify.ClassGUC, verify.ClassReloption} {
		p := predictionFromDetail(class, map[string]any{"estimated_improvement_pct": 50.0})
		if p.Predicts() {
			t.Errorf("class %q: prediction = %+v; GUC and reloption predictions come "+
				"from their metric, others have none", class, p)
		}
	}
}
