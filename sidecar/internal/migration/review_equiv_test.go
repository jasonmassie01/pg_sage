package migration_test

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/migration"
)

// No concurrent access tests: each engine is called once per statement.

// reviewEquivCorpus is the classifier_test.go corpus without DROP and
// TRUNCATE (agentguard classes any of those destructive before it
// classifies).
var reviewEquivCorpus = []string{
	"CREATE INDEX idx_foo ON bar (col)",
	"CREATE UNIQUE INDEX idx_u ON s.t (c)",
	"CREATE INDEX CONCURRENTLY idx_foo ON bar (col)",
	"ALTER TABLE t ADD CONSTRAINT ck_positive CHECK (val > 0)",
	"ALTER TABLE t ADD CONSTRAINT ck CHECK (v > 0) NOT VALID",
	"ALTER TABLE orders ADD CONSTRAINT fk_cust FOREIGN KEY (cust_id) REFERENCES customers(id)",
	"ALTER TABLE t ALTER COLUMN email SET NOT NULL",
	"ALTER TABLE t ALTER COLUMN price TYPE numeric(12,2)",
	"ALTER TABLE t ADD COLUMN id uuid DEFAULT gen_random_uuid()",
	"ALTER TABLE t ADD COLUMN status text DEFAULT 'active'",
	"REINDEX TABLE orders", "VACUUM FULL orders",
	"REFRESH MATERIALIZED VIEW mv_daily_stats",
	"CLUSTER orders USING idx_date",
	"ALTER TABLE orders SET TABLESPACE fast_ssd",
	"SET lock_timeout = '5s'; ALTER TABLE t ALTER COLUMN c SET NOT NULL",
	"CREATE TABLE t2 (id int)",
}

// Equivalence: over the corpus, Review, the advisor and agentguard agree.
// Review's highest-scoring rule and lock are the advisor's finding; and
// Review says safe exactly when agentguard classes the SQL additive.
func TestReviewAgreesWithAdvisorAndAgentguard(t *testing.T) {
	ctx := context.Background()
	adv := migration.NewAdvisor(nil, &config.MigrationConfig{}, 140000, "db",
		func(string, string, ...any) {}, nil)
	for _, sql := range reviewEquivCorpus {
		rep, err := migration.Review(ctx, nil, sql, migration.ReviewOptions{})
		if err != nil {
			t.Fatalf("Review(%q): %v", sql, err)
		}
		inc, risk, err := adv.AnalyzeRisk(ctx, sql)
		if err != nil {
			t.Fatalf("advisor(%q): %v", sql, err)
		}
		top := topHazard(rep)
		switch {
		case inc == nil && top != nil && top.RiskScore > 0.3:
			t.Errorf("%q: Review scores %s %.2f, the advisor reports nothing", sql,
				top.RuleID, top.RiskScore)
		case inc != nil && (top == nil || top.RuleID != risk.RuleID ||
			top.LockLevel != risk.LockLevel || top.RiskScore != risk.RiskScore):
			t.Errorf("%q: Review top %+v, advisor %s/%s/%.2f", sql, top, risk.RuleID,
				risk.LockLevel, risk.RiskScore)
		}
		additive := decide.ClassifyDDL(sql) == decide.CapDDLAdditive
		if additive != (rep.Verdict == migration.VerdictSafe) {
			t.Errorf("%q: Review %s, agentguard %s", sql, rep.Verdict, decide.ClassifyDDL(sql))
		}
	}
}

func topHazard(rep migration.Report) *migration.Hazard {
	var top *migration.Hazard
	for i := range rep.Statements {
		for j := range rep.Statements[i].Hazards {
			h := &rep.Statements[i].Hazards[j]
			if top == nil || h.RiskScore > top.RiskScore {
				top = h
			}
		}
	}
	return top
}
