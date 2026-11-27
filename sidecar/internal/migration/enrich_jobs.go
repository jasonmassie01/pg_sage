package migration

import (
	"context"

	"github.com/pg-sage/sidecar/internal/rca"
)

// The finding detail keys that carry model output (migration-10). Model
// text is shown here, labelled, and never becomes recommended_sql.
const (
	// detailLLMScript is the model's migration script, validated.
	detailLLMScript = "llm_script"
	// detailLLMSafeAlternative is the fallback's alternative, validated.
	detailLLMSafeAlternative = "llm_safe_alternative"
	// detailLLMInvalid is why pg_sage could not validate the model text.
	detailLLMInvalid = "llm_script_invalid"
	// detailLLMUnvalidated is the model text pg_sage could not validate.
	detailLLMUnvalidated = "llm_unvalidated_text"
)

// FindingDetailMerger replaces the model output of an open finding: it
// removes every llm_ key of the finding's detail and writes fields. The
// store implements it beside FindingSink.
type FindingDetailMerger interface {
	MergeMigrationFindingLLMDetail(
		ctx context.Context, identity string, fields map[string]any,
	) error
}

// RunEnrichment works the advisor's model queue until ctx is cancelled;
// without a model it returns at once.
func (a *Advisor) RunEnrichment(ctx context.Context) {
	if a == nil {
		return
	}
	a.enricher.Run(ctx)
}

// queueEnrichment queues the model work for a detection the caller has
// already persisted: the script for a classified incident, or the LLM
// fallback for a statement no rule classified. It never waits.
func (a *Advisor) queueEnrichment(
	pid int, sql string, inc *rca.Incident, risk *DDLRisk, sink FindingSink,
) {
	if a == nil || a.enricher == nil || sink == nil || !a.llmClient.IsEnabled() {
		return
	}
	job := enrichJob{pid: pid, sql: sql, sink: sink}
	switch {
	case inc != nil && risk != nil:
		job.risk = risk
		job.identity = migrationFindingIdentity(inc.DatabaseName, ruleIDFromIncident(inc),
			objectFromIncident(inc), sanitizeDDL(sql))
	case inc == nil && a.fallbackEligible(sanitizeDDL(sql)):
	default:
		return
	}
	a.enricher.enqueue(job)
}

// fallbackEligible reports whether the LLM fallback may assess clean: a
// model is on, no rule classified it, and its shape may leave the host.
func (a *Advisor) fallbackEligible(clean string) bool {
	return a.llmClient != nil && a.llmClient.IsEnabled() && llmEgressAllowed(clean) &&
		len(a.classifier.Classify(clean, a.pgVersion)) == 0
}

// runEnrichJob does one detection's model work and records the result.
func (a *Advisor) runEnrichJob(ctx context.Context, job enrichJob) {
	if job.risk != nil {
		res := a.scriptGen.GenerateScript(ctx, job.risk)
		a.mergeModelOutput(ctx, job.sink, job.identity, modelFields(res, detailLLMScript))
		return
	}
	inc, res := a.fallbackResult(ctx, job.sql)
	if inc == nil || job.sink == nil {
		return
	}
	finding, ok := FindingFromIncident(job.pid, job.sql, inc, nil)
	if !ok {
		return
	}
	for k, v := range modelFields(res, detailLLMSafeAlternative) {
		finding.Detail[k] = v
	}
	if _, err := job.sink.UpsertMigrationSafetyFinding(ctx, finding); err != nil {
		a.logFn("warn", "migration: persist LLM fallback finding failed: %v", err)
	}
}

// mergeModelOutput writes fields into the identified open finding.
func (a *Advisor) mergeModelOutput(
	ctx context.Context, sink FindingSink, identity string, fields map[string]any,
) {
	if len(fields) == 0 {
		return
	}
	m, ok := sink.(FindingDetailMerger)
	if !ok {
		a.logFn("warn", "migration: the finding store cannot record model output for %s",
			identity)
		return
	}
	if err := m.MergeMigrationFindingLLMDetail(ctx, identity, fields); err != nil {
		a.logFn("warn", "migration: record model output for %s failed: %v", identity, err)
	}
}

// modelFields are the detail keys for res: the validated text under
// validKey, or the unvalidated text with the reason; nil without either.
func modelFields(res ScriptResult, validKey string) map[string]any {
	switch {
	case res.Script != "":
		return map[string]any{validKey: res.Script}
	case res.ModelText != "":
		return map[string]any{detailLLMInvalid: res.Reason, detailLLMUnvalidated: res.ModelText}
	}
	return nil
}
