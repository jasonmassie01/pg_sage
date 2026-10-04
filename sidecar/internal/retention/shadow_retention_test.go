package retention

import (
	"context"
	"testing"
)

// Roadmap 1.4: shadow decisions age out on the actions window once they
// are scored (correct, incorrect, neutral or unscored); a pending decision
// is kept however old, so it can still be scored. The ledger's copy of
// counted scores (sage.trust_shadow_evidence) is promotion evidence and is
// exempt, like the real outcomes.

func TestPurgeRulesAgeShadowDecisionsOnceScored(t *testing.T) {
	for _, rule := range purgeRules(allDays(30)) {
		if rule.table != "shadow_decision" {
			continue
		}
		if rule.timeCol != "recorded_at" || rule.days != 30 ||
			rule.extra != "AND status <> 'pending'" {
			t.Fatalf("shadow_decision rule = %+v", rule)
		}
		return
	}
	t.Fatal("sage.shadow_decision has no purge rule")
}

func TestShadowEvidenceIsExempt(t *testing.T) {
	if retentionExemptions["trust_shadow_evidence"] == "" {
		t.Fatal("sage.trust_shadow_evidence must be documented as exempt promotion evidence")
	}
}

func TestRun_ShadowDecisionsAgeOutWhenScored(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("shadow_ret")
	seed := func(status, score, source, age string) int64 {
		return insertID(t, ctx, `INSERT INTO sage.shadow_decision (fingerprint, family,
			action_class, title, sql, shape, prediction, gate_verdict, gate_reason,
			trusted_verdict, trusted_reason, granted_level, status, score, score_source,
			scored_at, recorded_at, last_seen_at)
			VALUES ($1 || md5(random()::text), 'hygiene', 'vacuum', 't', 'VACUUM public.o',
			        'vacuum public.o', '{}', 'observe_only', 'autonomy_level', 'execute',
			        'autonomy_l3', 1, $2, NULLIF($3, ''), NULLIF($4, ''),
			        CASE WHEN $2 = 'scored' THEN now() END,
			        now() - $5::interval, now() - $5::interval)
			RETURNING id`, tag, status, score, source, age)
	}
	oldScored := seed("scored", "correct", "hypopg", "60 days")
	oldUnscored := seed("scored", "unscored", "none", "45 days")
	oldPending := seed("pending", "", "", "400 days")
	recentScored := seed("scored", "incorrect", "operator", "2 days")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM sage.shadow_decision WHERE fingerprint LIKE $1 || '%'`, tag)
	})
	New(testPool, allDays(30), noopLog).Run(ctx)
	exists := func(id int64) bool {
		return countWhere(t, ctx, `SELECT count(*) FROM sage.shadow_decision WHERE id = $1`,
			id) == 1
	}
	if exists(oldScored) || exists(oldUnscored) {
		t.Fatal("scored shadow decisions past the actions window were kept")
	}
	if !exists(oldPending) || !exists(recentScored) {
		t.Fatal("a pending or recent shadow decision was purged")
	}
}
