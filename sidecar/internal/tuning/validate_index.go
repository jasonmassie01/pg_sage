package tuning

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sqlident"
	"github.com/pg-sage/sidecar/internal/verify"
	"github.com/pg-sage/sidecar/internal/workload"
)

// singleStatement returns sql without one trailing semicolon, or false
// when it holds more than one statement.
func singleStatement(sql string) (string, bool) {
	s := strings.TrimSpace(sql)
	s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	return s, s != "" && !hasSemicolon(s)
}

// hasSemicolon reports a semicolon outside literals, comments and quoted
// identifiers.
func hasSemicolon(sql string) bool {
	t := workload.BlankLiteralsAndComments(sql)
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '"':
			_, next, _ := workload.ReadIdent(t, i)
			i = max(next, i+1) - 1
		case ';':
			return true
		}
	}
	return false
}

// judgeIndexCreate admits an index through the optimizer: its validator,
// rejection memory and HypoPG what-if. A verified measurement replaces
// the model's estimate.
func (v *validator) judgeIndexCreate(ctx context.Context, c Case, p Proposal) Judged {
	ddl, ok := singleStatement(p.DDL)
	if !ok {
		return reject(p, ReasonUnsupported, "an index proposal is one CREATE INDEX statement")
	}
	spec, err := optimizer.ParseIndexDDL(ddl)
	if err != nil {
		return reject(p, ReasonUnsupported, "not a CREATE INDEX statement: %v", err)
	}
	if spec.TableSchema == "" {
		return reject(p, ReasonInvalid, "the index's table must be schema-qualified")
	}
	table, j, ok := v.tableInCase(c, p, sqlident.Qualified(spec.TableSchema, spec.TableName))
	if !ok {
		return j
	}
	if _, why := predictedChange(p, true); why != "" {
		return reject(p, ReasonInvalid, "%s", why)
	}
	tc, ok := v.a.tableContext(ctx, v.cur, table)
	if !ok {
		return reject(p, ReasonUnavailable, "no table context for %s", table)
	}
	ov, bad := v.overlap(p, table, ddl, tc)
	if bad != nil {
		return *bad
	}
	var replaced collector.IndexStats
	if ov.replaces != nil {
		ix, j := v.replacement(p, spec, *ov.replaces)
		if j != nil {
			return *j
		}
		replaced = ix
	}
	adm := v.a.deps.Indexes.Admit(ctx, optimizer.Recommendation{DDL: ddl, Severity: "info",
		Rationale: p.Rationale, IndexType: spec.Method, Alongside: ov.alongside,
		Category: optimizer.OptimizerCategory, AffectedQueryIDs: v.targets(c, p),
		Replaces: replacedRef(replaced)}, tc)
	switch adm.Outcome {
	case optimizer.AdmitAccepted:
	case optimizer.AdmitMeasured:
		return reject(p, ReasonAlreadyMeasured, "%s", adm.Reason)
	case optimizer.AdmitRejected:
		return reject(p, ReasonWhatIfRejected, "%s", adm.Reason)
	default:
		return reject(p, ReasonInvalid, "%s", adm.Reason)
	}
	if ov.family != "" {
		v.flight.families[ov.family] = ddl
	}
	f := v.withRehearsal(analyzer.OptimizerRecommendationFinding(adm.Rec), ddl)
	pred := v.createPrediction(c, p, adm.Rec)
	if ov.replaces != nil {
		return v.admitReplacement(p, f, table, adm.Rec.DDL, replaced, pred)
	}
	return Judged{Proposal: p, Verdict: VerdictAdmitted, Finding: &f, Tables: []string{table},
		Class: verify.ClassIndexCreate, Prediction: pred}
}

// withRehearsal attaches the tier-2 record of the cycle's rehearsal of the
// same index, if it ran (lab-04a).
func (v *validator) withRehearsal(f analyzer.Finding, ddl string) analyzer.Finding {
	if rec, ok := v.tools.rehearsalOf(ddl); ok {
		f.Detail["ladder"] = verify.LadderValue(rec)
	}
	return f
}

// replacedRef is the index a replacement removes, for the optimizer's
// per-table index cap; nil for a plain create.
func replacedRef(ix collector.IndexStats) *optimizer.IndexRef {
	if ix.IndexRelName == "" {
		return nil
	}
	return &optimizer.IndexRef{Name: ix.IndexRelName, OID: ix.IndexRelID}
}

func (v *validator) createPrediction(c Case, p Proposal,
	rec optimizer.Recommendation) verify.Prediction {
	pred := verify.Prediction{Class: verify.ClassIndexCreate, Method: verify.MethodModel,
		Metric: verify.MetricMeanExecTime, ExpectedChangePct: p.ExpectedChangePct,
		Source: PredictionSource, Note: "the model's estimate"}
	if rec.WhatIf == optimizer.WhatIfVerified {
		measured := -rec.EstimatedImprovementPct
		pred.Method, pred.ExpectedChangePct = verify.MethodHypoPG, &measured
		pred.Note = "HypoPG's call-weighted cost reduction"
	}
	var ids []int64
	for _, id := range rec.AffectedQueryIDs {
		if v.w.IsWorkload(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		ids = v.targets(c, p)
	}
	pred.TargetQueryIDs = ids
	return pred
}

// judgeIndexDrop admits the drop of an index that another valid index
// covers. Unique and primary-key indexes are never dropped, and a
// never-scanned index is the unused_index rule's (indexes-03). The drop
// carries the rules engine's standby gate: primary statistics miss replica
// reads. The drop is measured by hiding the index (HypoPG 1.4+): a guarded
// statement that regresses rejects it, a verified measurement becomes the
// prediction, and an unmeasured drop keeps the rule's "reads unchanged".
func (v *validator) judgeIndexDrop(ctx context.Context, c Case, p Proposal) Judged {
	schema, name, ok := splitQualified(p.Index)
	if !ok {
		return reject(p, ReasonInvalid, "%q is not a schema-qualified index", p.Index)
	}
	ix, found := v.findIndex(schema, name)
	if !found {
		return reject(p, ReasonInvalid, "no index %s", sqlident.Qualified(schema, name))
	}
	table, j, ok := v.tableInCase(c, p, sqlident.Qualified(ix.SchemaName, ix.RelName))
	if !ok {
		return j
	}
	by, why := v.coveredBy(ix)
	if by == "" {
		return reject(p, ReasonInvalid, "%s", why)
	}
	m := v.measureDrop(ctx, ix, table)
	if m.Verdict == optimizer.WhatIfRejected {
		return reject(p, ReasonWhatIfRejected, "%s", m.Reason)
	}
	v.dropping[ix.IndexRelName] = true
	f := analyzer.WithStandbyGate(dropFinding(ix, table, p, by),
		analyzer.StandbyUsageUnknown(v.cur))
	f.Detail["what_if_verdict"] = m.Verdict
	if m.Reason != "" {
		f.Detail["what_if_reason"] = m.Reason
	}
	return Judged{Proposal: p, Verdict: VerdictAdmitted, Finding: &f, Tables: []string{table},
		Class: verify.ClassIndexDrop, Prediction: dropPrediction(v.targets(c, p), m)}
}

// measureDrop measures dropping ix through the optimizer; an index with no
// identity (OID) or table context cannot be measured.
func (v *validator) measureDrop(ctx context.Context, ix collector.IndexStats,
	table string) optimizer.DropMeasurement {
	if ix.IndexRelID == 0 {
		return optimizer.DropMeasurement{Verdict: optimizer.WhatIfUnverified,
			Reason: "the index's identity (OID) is not in the snapshot"}
	}
	tc, ok := v.a.tableContext(ctx, v.cur, table)
	if !ok {
		return optimizer.DropMeasurement{Verdict: optimizer.WhatIfUnverified,
			Reason: "no table context for " + table}
	}
	return v.a.deps.Indexes.MeasureDrop(ctx,
		optimizer.IndexRef{Name: ix.IndexRelName, OID: ix.IndexRelID}, tc)
}

// dropPrediction is a verified measurement's change (model), or the rule's
// "reads unchanged" with why the drop was not measured.
func dropPrediction(targets []int64, m optimizer.DropMeasurement) verify.Prediction {
	pred := verify.Prediction{Class: verify.ClassIndexDrop, Method: verify.MethodRule,
		Metric: verify.MetricMeanExecTime, TargetQueryIDs: targets,
		Source: PredictionSource}
	change := 0.0
	if m.Verdict == optimizer.WhatIfVerified {
		change = m.ChangePct
		pred.Method = verify.MethodModel
		pred.Note = "HypoPG's call-weighted planned-cost change with the index hidden; " +
			"the index's writes and space are freed"
	} else {
		pred.Note = "reads are expected unchanged; the index's writes and space are " +
			"freed (not measured: " + m.Reason + ")"
	}
	pred.ExpectedChangePct = &change
	return pred
}

func (v *validator) findIndex(schema, name string) (collector.IndexStats, bool) {
	for _, ix := range v.cur.Indexes {
		if ix.SchemaName == schema && ix.IndexRelName == name {
			return ix, true
		}
	}
	return collector.IndexStats{}, false
}

// coveredBy is the valid index that covers ix, or "" with why ix may not
// be dropped. Tuning never decides that an index is unused: the
// unused_index rule owns those drops, with its first-seen clock, its
// recently-created and FK-only skips and its standby gate.
func (v *validator) coveredBy(ix collector.IndexStats) (string, string) {
	if ix.IsUnique || ix.IsPrimary {
		return "", fmt.Sprintf("%s enforces uniqueness and is never dropped", ix.IndexRelName)
	}
	if !ix.IsValid {
		return "", fmt.Sprintf("%s is invalid; the invalid-index rule handles it",
			ix.IndexRelName)
	}
	var others []collector.IndexStats
	for _, o := range v.cur.Indexes {
		if o.SchemaName == ix.SchemaName && o.RelName == ix.RelName &&
			!v.dropping[o.IndexRelName] {
			others = append(others, o)
		}
	}
	if by := coveringIndex(ix, others); by != "" {
		return by, ""
	}
	if ix.IdxScan > 0 {
		return "", fmt.Sprintf("%s was scanned %d times and no other index covers it",
			ix.IndexRelName, ix.IdxScan)
	}
	return "", fmt.Sprintf("%s is never scanned and no other index covers it; the "+
		"unused_index rule owns unused drops (first-seen clock, FK and standby guards)",
		ix.IndexRelName)
}

func dropFinding(ix collector.IndexStats, table string, p Proposal,
	coveredBy string) analyzer.Finding {
	index := sqlident.Qualified(ix.SchemaName, ix.IndexRelName)
	d := map[string]any{"table": table, "index": index, "index_bytes": ix.IndexBytes,
		"index_scans": ix.IdxScan, "action_risk": optimizer.RiskHigh,
		"covered_by": coveredBy}
	reason := fmt.Sprintf("%s is covered by %s", index, coveredBy)
	return analyzer.Finding{Category: CategoryIndexDrop, Severity: "info",
		ObjectType: "index", ObjectIdentifier: index,
		Title:          fmt.Sprintf("Drop index %s on %s", index, table),
		Detail:         d,
		Recommendation: strings.TrimSpace(p.Rationale + " (" + reason + ")"),
		RecommendedSQL: "DROP INDEX CONCURRENTLY IF EXISTS " + index,
		RollbackSQL:    createConcurrently(ix.IndexDef), ActionRisk: optimizer.RiskHigh}
}

// createConcurrently turns a pg_get_indexdef definition into its
// CONCURRENTLY form, the rollback of a drop.
func createConcurrently(def string) string {
	for _, prefix := range []string{"CREATE UNIQUE INDEX ", "CREATE INDEX "} {
		if rest, ok := strings.CutPrefix(def, prefix); ok {
			return prefix + "CONCURRENTLY " + rest
		}
	}
	return def
}
