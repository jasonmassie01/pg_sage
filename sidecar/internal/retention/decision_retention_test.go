package retention

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Non-execute decisions (parked, queued, blocked, observe-only: the gate's
// withheld verdicts and the schema guard's observations) age out on
// retention.decisions_days (default 30), measured from when the verdict was
// last seen. A row an action, a verification, a lease or a value record
// refers to is kept, as is a schema guard retention dry run (the evidence
// that authorizes later deletes). Execute decisions keep the actions window.
func decisionRetention(decisionsDays, actionsDays int) *config.Config {
	return &config.Config{Retention: config.RetentionConfig{
		SnapshotsDays: 30, FindingsDays: 30, ActionsDays: actionsDays,
		ExplainsDays: 30, DecisionsDays: decisionsDays,
	}}
}

type decisionSeed struct {
	feature, verdict, age, lastSeen, disposition string
}

func insertSeedDecision(t *testing.T, ctx context.Context, tag string, s decisionSeed) int64 {
	t.Helper()
	return insertID(t, ctx, `INSERT INTO sage.decision
		(feature, intent, verdict, risk_tier, reason, evidence_id, target_objects,
		 evidence, created_at, last_seen_at)
		VALUES ($1, 'missing_fk_index', $2, 'moderate', 'test',
		        $3 || '_' || md5(random()::text), jsonb_build_array($3::text),
		        CASE WHEN $4 = '' THEN '{}'::jsonb
		             ELSE jsonb_build_object('disposition', $4::text) END,
		        now() - $5::interval,
		        CASE WHEN $6 = '' THEN NULL ELSE now() - $6::interval END)
		RETURNING id`, s.feature, s.verdict, tag, s.disposition, s.age, s.lastSeen)
}

func decisionExists(t *testing.T, ctx context.Context, id int64) bool {
	t.Helper()
	return countWhere(t, ctx, `SELECT count(*) FROM sage.decision WHERE id=$1`, id) == 1
}

func TestRun_NonExecuteDecisionsAgeOutOnTheDecisionsWindow(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("dec_ret")
	seed := func(s decisionSeed) int64 { return insertSeedDecision(t, ctx, tag, s) }
	purged := map[string]int64{
		"old parked":       seed(decisionSeed{"index", "parked", "60 days", "", ""}),
		"old observe_only": seed(decisionSeed{"schema_guard", "observe_only", "60 days", "", ""}),
		"old blocked":      seed(decisionSeed{"vacuum", "blocked", "45 days", "40 days", ""}),
		"ancient queued":   seed(decisionSeed{"index", "queue_approval", "400 days", "", ""}),
	}
	kept := map[string]int64{
		"recent parked":  seed(decisionSeed{"index", "parked", "1 day", "", ""}),
		"still repeated": seed(decisionSeed{"index", "parked", "90 days", "1 hour", ""}),
		"old execute":    seed(decisionSeed{"index", "execute", "60 days", "", ""}),
		"dry run":        seed(decisionSeed{"schema_guard", "observe_only", "60 days", "", "dry_run"}),
		"backs action":   seed(decisionSeed{"index", "parked", "60 days", "", ""}),
		"backs verify":   seed(decisionSeed{"index", "parked", "60 days", "", ""}),
	}
	insertID(t, ctx, `INSERT INTO sage.action_log (action_type, sql_executed, outcome,
		decision_id) VALUES ($1, 'SELECT 1', 'success', $2) RETURNING id`,
		tag, kept["backs action"])
	actionID := insertID(t, ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome) VALUES ($1, 'SELECT 1', 'success') RETURNING id`, tag)
	insertID(t, ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict)
		VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 1, now(), now(), 'pending')
		RETURNING id`, kept["backs verify"], actionID)

	New(testPool, decisionRetention(30, 365), noopLog).Run(ctx)

	for name, id := range purged {
		if decisionExists(t, ctx, id) {
			t.Errorf("%s decision was kept past the decisions window", name)
		}
	}
	for name, id := range kept {
		if !decisionExists(t, ctx, id) {
			t.Errorf("%s decision was purged", name)
		}
	}
}

// decisions_days = 0 disables the rule (like every rule): the general
// decision rule (actions window) still applies.
func TestRun_DecisionsRuleDisabledWithZeroDays(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("dec_ret0")
	old := insertSeedDecision(t, ctx, tag, decisionSeed{"index", "parked", "60 days", "", ""})
	ancient := insertSeedDecision(t, ctx, tag,
		decisionSeed{"index", "parked", "400 days", "", ""})

	New(testPool, decisionRetention(0, 365), noopLog).Run(ctx)

	if !decisionExists(t, ctx, old) {
		t.Error("decisions_days=0 still purged a 60-day parked decision")
	}
	if decisionExists(t, ctx, ancient) {
		t.Error("the actions window no longer purges parked decisions")
	}
}

func TestPurgeRulesHaveANonExecuteDecisionRule(t *testing.T) {
	cfg := decisionRetention(21, 365)
	for _, rule := range purgeRules(cfg) {
		if rule.table != "decision" || !strings.Contains(rule.extra, "verdict <> 'execute'") {
			continue
		}
		if rule.days != 21 || !strings.Contains(rule.timeCol, "last_seen_at") ||
			!strings.Contains(rule.extra, "sage.action_log") ||
			!strings.Contains(rule.extra, "sage.verification") ||
			!strings.Contains(rule.extra, "dry_run") {
			t.Fatalf("non-execute decision rule = %+v, want the decisions window on "+
				"last_seen_at with the action, verification and dry-run keeps", rule)
		}
		return
	}
	t.Fatal("no purge rule for non-execute decisions")
}
