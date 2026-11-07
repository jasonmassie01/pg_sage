//go:build !cgo

package broker

import (
	"context"
	"testing"
)

// Without the SQL parser no agent statement can be verified, so an agent's
// EXPLAIN is refused, audited, and never logs in (fail closed).
func TestExplainWithoutParserIsBlocked(t *testing.T) {
	deps := boundDeps()
	audit := deps.Audit.(*memAudit)
	b := newFakeBroker(t, deps)
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	res, err := b.Explain(ctx, ExplainRequest{Database: "db", SQL: "SELECT 1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonParserUnavailable ||
		len(res.PlanJSON) != 0 {
		t.Errorf("result = %+v, want blocked %s and no plan", res, ReasonParserUnavailable)
	}
	if deps.Logins.(*fakeLogins).calls != 0 {
		t.Error("a broker login was opened for an unverified statement")
	}
	recs := audit.all()
	if len(recs) != 1 || recs[0].Reason != ReasonParserUnavailable || recs[0].RowCount != nil {
		t.Errorf("audit = %+v, want one blocked row without a row count", recs)
	}
}
