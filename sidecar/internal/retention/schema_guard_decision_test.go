package retention

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Schema guard decisions are observations (observe_only and parked rows,
// one per changed invariant decision): they age out on the findings
// window, not the year-long actions window. Rows that back an action or a
// verification are kept, however old.
func schemaGuardRetention(findingsDays, actionsDays int) *config.Config {
	return &config.Config{Retention: config.RetentionConfig{
		SnapshotsDays: 30, FindingsDays: findingsDays, ActionsDays: actionsDays,
		ExplainsDays: 30,
	}}
}

func insertGuardDecision(
	t *testing.T, ctx context.Context, feature, tag, age string,
) int64 {
	t.Helper()
	return insertID(t, ctx, `INSERT INTO sage.decision
		(feature, intent, verdict, risk_tier, reason, evidence_id, target_objects,
		 created_at)
		VALUES ($1, 'missing_fk_index', 'observe_only', 'moderate', 'test',
		        $2 || '_' || md5(random()::text), jsonb_build_array($2::text),
		        now() - $3::interval) RETURNING id`, feature, tag, age)
}

func decisionExists(t *testing.T, ctx context.Context, id int64) bool {
	t.Helper()
	return countWhere(t, ctx, `SELECT count(*) FROM sage.decision WHERE id=$1`, id) == 1
}

func TestRun_SchemaGuardDecisionsAgeOutOnTheFindingsWindow(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("sg_ret")
	old := insertGuardDecision(t, ctx, "schema_guard", tag, "60 days")
	ancient := insertGuardDecision(t, ctx, "schema_guard", tag, "400 days")
	recent := insertGuardDecision(t, ctx, "schema_guard", tag, "1 day")
	backsAction := insertGuardDecision(t, ctx, "schema_guard", tag, "60 days")
	backsVerification := insertGuardDecision(t, ctx, "schema_guard", tag, "60 days")
	otherFeature := insertGuardDecision(t, ctx, "fk_index", tag, "60 days")
	insertID(t, ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, decision_id)
		VALUES ($1, 'SELECT 1', 'success', $2) RETURNING id`, tag, backsAction)
	actionID := insertID(t, ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ($1, 'SELECT 1', 'success')
		RETURNING id`, tag)
	insertID(t, ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict)
		VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 1, now(), now(), 'pending')
		RETURNING id`, backsVerification, actionID)

	New(testPool, schemaGuardRetention(30, 365), noopLog).Run(ctx)

	if decisionExists(t, ctx, old) || decisionExists(t, ctx, ancient) {
		t.Error("schema guard decisions past the findings window were kept")
	}
	if !decisionExists(t, ctx, recent) {
		t.Error("a recent schema guard decision was purged")
	}
	if !decisionExists(t, ctx, backsAction) {
		t.Error("a schema guard decision backing an action was purged")
	}
	if !decisionExists(t, ctx, backsVerification) {
		t.Error("a schema guard decision backing a verification was purged")
	}
	if !decisionExists(t, ctx, otherFeature) {
		t.Error("a non-schema-guard decision was purged on the findings window")
	}
}

// findings_days = 0 disables the schema guard rule (like every rule): the
// general decision rule (actions window) still applies.
func TestRun_SchemaGuardRuleDisabledWithZeroFindingsDays(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("sg_ret0")
	old := insertGuardDecision(t, ctx, "schema_guard", tag, "60 days")
	ancient := insertGuardDecision(t, ctx, "schema_guard", tag, "400 days")

	New(testPool, schemaGuardRetention(0, 365), noopLog).Run(ctx)

	if !decisionExists(t, ctx, old) {
		t.Error("findings_days=0 still purged a 60-day schema guard decision")
	}
	if decisionExists(t, ctx, ancient) {
		t.Error("the actions window no longer purges schema guard decisions")
	}
}

func TestPurgeRulesHaveASchemaGuardDecisionRule(t *testing.T) {
	cfg := schemaGuardRetention(45, 365)
	for _, rule := range purgeRules(cfg) {
		if rule.table != "decision" || !strings.Contains(rule.extra, "schema_guard") {
			continue
		}
		if rule.days != 45 || rule.timeCol != "created_at" ||
			!strings.Contains(rule.extra, "sage.action_log") ||
			!strings.Contains(rule.extra, "sage.verification") {
			t.Fatalf("schema guard rule = %+v, want the findings window and the "+
				"action/verification keeps", rule)
		}
		return
	}
	t.Fatal("no purge rule for schema guard decisions")
}
