package retention

import (
	"context"
	"testing"
)

// Roadmap 2.3: a confirmed or proposed fact is never purged (confirmed
// facts are binding memory); a rejected or expired fact ages out of the
// actions window from its last change. Fact cards age from creation.

func TestPurgeRulesKeepConfirmedFacts(t *testing.T) {
	found := map[string]bool{}
	for _, rule := range purgeRules(allDays(30)) {
		switch rule.table {
		case "facts":
			if rule.timeCol != "updated_at" || rule.days != 30 ||
				rule.extra != "AND status IN ('rejected', 'expired')" {
				t.Fatalf("facts rule = %+v", rule)
			}
			found[rule.table] = true
		case "fact_card_deliveries":
			if rule.timeCol != "created_at" || rule.days != 30 {
				t.Fatalf("fact_card_deliveries rule = %+v", rule)
			}
			found[rule.table] = true
		}
	}
	if !found["facts"] || !found["fact_card_deliveries"] {
		t.Fatalf("purge rules found: %v", found)
	}
}

func TestRun_FactsAgeOutOnlyWhenRejectedOrExpired(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("facts_ret")
	seed := func(status, age string) int64 {
		return insertID(t, ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
			source, status, decided_by, decided_at, updated_at)
			VALUES ('test_fixture', 'schema', $1 || '_' || md5(random()::text), 'operator',
			        $2, CASE WHEN $2 IN ('confirmed', 'rejected') THEN 'op' END,
			        CASE WHEN $2 IN ('confirmed', 'rejected') THEN now() END,
			        now() - $3::interval)
			RETURNING id`, tag, status, age)
	}
	oldConfirmed := seed("confirmed", "400 days")
	oldProposed := seed("proposed", "400 days")
	oldRejected := seed("rejected", "60 days")
	oldExpired := seed("expired", "60 days")
	newRejected := seed("rejected", "1 day")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			"DELETE FROM sage.facts WHERE subject LIKE $1 || '%'", tag)
	})
	New(testPool, allDays(30), noopLog).Run(ctx)
	for id, kept := range map[int64]bool{oldConfirmed: true, oldProposed: true,
		oldRejected: false, oldExpired: false, newRejected: true} {
		got := countWhere(t, ctx, `SELECT count(*) FROM sage.facts WHERE id = $1`, id) == 1
		if got != kept {
			t.Fatalf("fact %d kept=%v, want %v", id, got, kept)
		}
	}
}
