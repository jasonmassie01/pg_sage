package tuning

import (
	"fmt"
	"maps"
	"strconv"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/verify"
)

// finish applies what binds every admitted proposal: an operator's earlier
// rejection, the confirmed facts (a bound change becomes a source-fix
// packet) and the agent's provenance in the finding detail.
func (v *validator) finish(c Case, ev evidenceSet, j Judged) Judged {
	f := *j.Finding
	if v.rejected[normalizeSQL(f.RecommendedSQL)] {
		return reject(j.Proposal, ReasonOperatorRejected, "an operator rejected %s",
			f.RecommendedSQL)
	}
	out, _ := facts.ApplyFindings(v.confirmed, []analyzer.Finding{f}, nil, v.a.now())
	if len(out) == 0 {
		return reject(j.Proposal, ReasonNotWorkload, "a confirmed fact marks %s a test "+
			"fixture", f.ObjectIdentifier)
	}
	f = out[0]
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	if f.RecommendedSQL == "" && f.Detail["source_fix"] != nil {
		j.Verdict = VerdictRedirected
		delete(f.Detail, analyzer.DetailApprovalRequired)
	}
	decorate(&f, c, ev, j)
	j.Finding = &f
	return j
}

// decorate records the agent's provenance: producer, case, proposal type,
// the cited evidence and the prediction the executor records.
func decorate(f *analyzer.Finding, c Case, ev evidenceSet, j Judged) {
	f.Detail["producer"] = Producer
	f.Detail["case_id"] = c.ID
	f.Detail["case_kind"] = string(c.Kind)
	f.Detail["proposal_type"] = string(j.Proposal.Type)
	if _, has := f.Detail["llm_rationale"]; !has && j.Proposal.Rationale != "" {
		// "rationale", not "llm_rationale": the latter marks index advice.
		f.Detail["rationale"] = j.Proposal.Rationale
	}
	cited := make([]Evidence, 0, len(j.Proposal.Evidence))
	for _, id := range j.Proposal.Evidence {
		if e, ok := ev[id]; ok {
			cited = append(cited, e)
		}
	}
	f.Detail["evidence"] = cited
	f.Detail["predicted_effect"] = predictionMap(j.Prediction)
	if f.ActionRisk != "" {
		f.Detail["action_risk"] = f.ActionRisk
	}
}

// predictionMap is a prediction as the finding detail carries it.
// Queryids are decimal strings: they survive a JSON round trip that
// decodes numbers as float64 (the executor parses them back).
func predictionMap(p verify.Prediction) map[string]any {
	ids := make([]string, len(p.TargetQueryIDs))
	for i, id := range p.TargetQueryIDs {
		ids[i] = strconv.FormatInt(id, 10)
	}
	m := map[string]any{"class": p.Class, "method": p.Method, "metric": p.Metric,
		"target_queryids": ids, "source": p.Source, "note": p.Note}
	if p.ExpectedChangePct != nil {
		m["expected_change_pct"] = *p.ExpectedChangePct
	}
	return m
}

// applyConfidence records the calibrated confidence on an admitted
// finding. Below the threshold (by the interval's lower bound) an
// operator must approve; an uncalibrated finding carries no number.
func applyConfidence(f *analyzer.Finding, conf Confidence, minOutcomes int,
	threshold float64, redirected bool) {
	cal := map[string]any{"status": conf.Status, "basis": conf.Basis, "n": conf.N,
		"hits": conf.Hits, "bin": conf.Bin, "min_outcomes": minOutcomes}
	if conf.Value != nil {
		cal["value"] = *conf.Value
		cal["wilson_low"], cal["wilson_high"] = conf.WilsonLow, conf.WilsonHigh
		f.Detail["confidence_score"] = *conf.Value
	}
	f.Detail["confidence_calibration"] = cal
	if redirected || conf.Status != StatusCalibrated || conf.WilsonLow >= threshold {
		return
	}
	if _, already := f.Detail[analyzer.DetailApprovalRequired]; already {
		return
	}
	f.Detail[analyzer.DetailApprovalRequired] = fmt.Sprintf("calibrated confidence is "+
		"below %.0f%%: %d of %d comparable actions improved", threshold*100, conf.Hits,
		conf.N)
}
