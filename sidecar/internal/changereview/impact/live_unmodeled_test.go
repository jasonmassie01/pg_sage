//go:build cgo

package impact

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// "Unknown is never none": a statement kind the engine has no rule for
// is a CR-17 warning (review), and the destructive ones it can judge are
// critical: TRUNCATE of a table with rows, DROP SCHEMA ... CASCADE of a
// schema holding relations (and DROP SCHEMA without CASCADE, which
// fails). Settings, comments and the like stay safe.

func TestUnmodeledStatementsAreNeverSafe(t *testing.T) {
	e := newEnv(t)
	e.f.Exec(`CREATE FUNCTION rv.touch() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN RETURN NEW; END $$`)
	e.f.Exec(`CREATE SEQUENCE rv.seq_unused`)
	cases := []struct {
		sql, verdict, rule, severity, says string
	}{
		{"TRUNCATE rv.rv_orders", changereview.VerdictRisky, RuleLockAndRewrite,
			changereview.SeverityCritical, "removes every row"},
		{"TRUNCATE rv.rv_orders, rv.rv_items CASCADE", changereview.VerdictRisky,
			RuleLockAndRewrite, changereview.SeverityCritical, "rv.rv_items"},
		{"DROP SCHEMA rv CASCADE", changereview.VerdictRisky, RuleRelationReferenced,
			changereview.SeverityCritical, "rv.rv_orders"},
		{"DROP SCHEMA rv", changereview.VerdictRisky, RuleWillFail,
			changereview.SeverityCritical, "will fail"},
		{"DROP FUNCTION rv.touch()", changereview.VerdictReview, RuleImpactUnknown,
			changereview.SeverityWarning, "not modeled"},
		{"DROP SEQUENCE rv.seq_unused CASCADE", changereview.VerdictReview,
			RuleImpactUnknown, changereview.SeverityWarning, "CASCADE"},
		{"CREATE TRIGGER rv_orders_touch BEFORE UPDATE ON rv.rv_orders FOR EACH ROW " +
			"EXECUTE FUNCTION rv.touch()", changereview.VerdictReview, RuleImpactUnknown,
			changereview.SeverityWarning, "not modeled"},
		{"ALTER INDEX rv.rv_orders_created_idx RENAME TO rv_orders_created_at_idx",
			changereview.VerdictReview, RuleImpactUnknown, changereview.SeverityWarning,
			"not modeled"},
		{"ALTER TABLE rv.rv_orders SET UNLOGGED", changereview.VerdictReview,
			RuleImpactUnknown, changereview.SeverityWarning, "not modeled"},
		{"DO $$ BEGIN PERFORM 1; END $$", changereview.VerdictReview, RuleImpactUnknown,
			changereview.SeverityWarning, "not modeled"},
	}
	for _, c := range cases {
		a := e.analyze(c.sql)
		if got := verdict(a); got != c.verdict {
			t.Errorf("%q: verdict %s, want %s:%s", c.sql, got, c.verdict, describe(a))
			continue
		}
		found := false
		for _, f := range byRule(a, c.rule) {
			if f.Severity == c.severity && strings.Contains(f.Title+" "+f.Text, c.says) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: want a %s %s finding saying %q:%s", c.sql, c.severity, c.rule,
				c.says, describe(a))
		}
	}
}

func TestModeledOtherStatementsStaySafe(t *testing.T) {
	e := newEnv(t)
	for _, sql := range []string{
		"COMMENT ON TABLE rv.rv_orders IS 'orders'",
		"ALTER TABLE rv.rv_orders SET (fillfactor = 90)",
		"ALTER TABLE rv.rv_orders ALTER COLUMN status SET STATISTICS 500",
		"CREATE SEQUENCE rv.new_seq",
		"CREATE SCHEMA rv_new",
		"SET lock_timeout = '2s'",
	} {
		a := e.analyze(sql)
		none(t, a, RuleImpactUnknown, changereview.RuleTextMode)
		if got := verdict(a); got == changereview.VerdictRisky {
			t.Errorf("%q: verdict %s:%s", sql, got, describe(a))
		}
	}
}

// TRUNCATE takes ACCESS EXCLUSIVE: the statements on the table queue
// behind it (CR-02), reads included.
func TestTruncateQueuesTheWorkload(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("TRUNCATE rv.rv_orders")
	_, q := lockQueueOf(t, a)
	if q.LockMode != lockAccessExclusive || len(q.ConflictingQueryIDs) != 3 {
		t.Fatalf("TRUNCATE CR-02 %+v, want ACCESS EXCLUSIVE blocking Q1..Q3", q)
	}
	b := e.analyze("LOCK TABLE rv.rv_orders IN SHARE MODE")
	_, lock := lockQueueOf(t, b)
	if lock.LockMode != lockShare || len(lock.ConflictingQueryIDs) != 1 ||
		lock.ConflictingQueryIDs[0] != e.f.Q2.ID {
		t.Fatalf("LOCK ... IN SHARE MODE CR-02 %+v, want SHARE blocking only Q2", lock)
	}
	none(t, b, RuleImpactUnknown)
}

// An empty table is not data loss: TRUNCATE of it is a warning at most.
func TestTruncateOfAnEmptyTableIsNotCritical(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("CREATE TABLE rv.empty_t (id bigint PRIMARY KEY)")
	e.f.Exec("ANALYZE rv.empty_t")
	a := e.analyze("TRUNCATE rv.empty_t")
	for _, f := range a.Findings {
		if f.Severity == changereview.SeverityCritical {
			t.Fatalf("TRUNCATE of an empty table: critical %s %q", f.RuleID, f.Title)
		}
	}
}

// A model statement of an unmodeled kind is inspected like any other:
// InspectStatement reports the CR-17 finding (the reasoner raises it).
func TestInspectUnmodeledStatement(t *testing.T) {
	e := newEnv(t)
	ins, err := e.eng.InspectStatement(e.ctx, "TRUNCATE rv.rv_orders",
		changereview.OriginModelTranslated)
	if err != nil || !ins.Parsed {
		t.Fatalf("InspectStatement: %+v, %v", ins, err)
	}
	crit := false
	for _, f := range ins.Findings {
		crit = crit || (f.RuleID == RuleLockAndRewrite &&
			f.Severity == changereview.SeverityCritical)
	}
	if !crit || ins.Statement.Kind != "truncate" {
		t.Fatalf("inspection of TRUNCATE: kind %s findings %+v", ins.Statement.Kind,
			ins.Findings)
	}
}
