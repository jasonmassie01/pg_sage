package agenttools

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// classSourceFix is the prediction class of a change that is not an index.
const classSourceFix = "source_fix"

const verifyMethod = "before/after comparison of the target queries' call-weighted " +
	"mean_exec_time in sage.query_store (Welch test with practical bars)"

// verificationPlan is how pg_sage will judge the deploy of change.
func (t *Tools) verificationPlan(f finding, change Change, ids []int64, index *indexRef,
) VerificationPlan {
	class := classSourceFix
	if index != nil {
		class = verify.ClassIndexCreate
	}
	pred := findingPrediction(f, class, ids)
	minutes := int(t.opts.VerifyWindow / time.Minute)
	v := VerificationPlan{Method: verifyMethod, Metric: verify.MetricMeanExecTime,
		WindowMinutes: minutes, Prediction: pred}
	if pred.ExpectedChangePct != nil {
		expected := *pred.ExpectedChangePct
		v.ExpectedChangePct = &expected
	}
	v.Steps = verificationSteps(change, len(ids), minutes, index)
	return v
}

// findingPrediction turns the finding's estimated improvement into a
// predicted change of mean_exec_time; HypoPG-validated estimates are
// method hypopg, others model. Without an estimate there is none.
func findingPrediction(f finding, class string, ids []int64) verify.Prediction {
	source := fmt.Sprintf("sage.findings#%d", f.ID)
	pct, ok := detailNumber(f.Detail["estimated_improvement_pct"])
	if !ok || !(pct > 0 && pct <= 100) {
		p := verify.NoPrediction(class, "the finding carries no estimated improvement")
		p.Metric, p.TargetQueryIDs, p.Source = verify.MetricMeanExecTime, ids, source
		return p
	}
	method := verify.MethodModel
	if validated, _ := f.Detail["hypopg_validated"].(bool); validated {
		method = verify.MethodHypoPG
	}
	expected := -pct
	p := verify.Prediction{Class: class, Method: method, Metric: verify.MetricMeanExecTime,
		TargetQueryIDs: ids, ExpectedChangePct: &expected, Source: source}
	if base, ok := detailNumber(f.Detail["mean_exec_time"]); ok && base > 0 {
		p.Baseline = &base
	}
	return p
}

func verificationSteps(change Change, targets, minutes int, index *indexRef) []string {
	steps := []string{"Add change.up to the application's migrations as a new migration " +
		"(pg_sage will not run it); change.down is its rollback."}
	if change.NonTransactional {
		steps = append(steps, "Make that migration non-transactional: CONCURRENTLY "+
			"cannot run inside a transaction.")
	}
	steps = append(steps,
		"Open the pull request, then call report_source_fix with stage pr_opened, the "+
			"pull request URL and this packet's hash.",
		"After the deploy, call report_source_fix with stage deployed and deployed_at.")
	if index != nil {
		steps = append(steps, fmt.Sprintf("The index %s must exist and be valid after the "+
			"deploy, or the verdict is unverifiable.", safeObject(index.String())))
	}
	if targets == 0 {
		return append(steps, "The finding names no target queries, so the verdict will "+
			"be insufficient_evidence or unverifiable.")
	}
	return append(steps, fmt.Sprintf("pg_sage compares mean_exec_time of the %d target "+
		"queries over %d minutes before and after deployed_at; call report_source_fix "+
		"with stage status after that window for the verdict.", targets, minutes))
}
